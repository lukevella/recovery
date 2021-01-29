package libwallet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"

	"github.com/muun/libwallet/aescbc"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil/base58"
)

const serializedPublicKeyLength = btcec.PubKeyBytesLenCompressed
const PKEncryptionVersion = 1

// maxDerivationPathLen is a safety limit to avoid stupid size allocations
const maxDerivationPathLen = 1000

// maxSignatureLen is a safety limit to avoid giant allocations
const maxSignatureLen = 200

// minNonceLen is the safe minimum we'll set for the nonce. This is the default for golang, but it's not exposed.
const minNonceLen = 12

type Encrypter interface {
	// Encrypt the payload and return a string with the necesary information for decryption
	Encrypt(payload []byte) (string, error)
}

type Decrypter interface {
	// Decrypt a payload generated by Encrypter
	Decrypt(payload string) ([]byte, error)
}

type hdPubKeyEncrypter struct {
	receiverKey *HDPublicKey
	senderKey   *HDPrivateKey
}

func addVariableBytes(writer io.Writer, data []byte) error {
	if len(data) > math.MaxUint16 {
		return fmt.Errorf("data length can't exceeed %v", math.MaxUint16)
	}

	dataLen := uint16(len(data))
	err := binary.Write(writer, binary.BigEndian, &dataLen)
	if err != nil {
		return fmt.Errorf("failed to write var bytes len: %w", err)
	}

	n, err := writer.Write(data)
	if err != nil || n != len(data) {
		return errors.New("failed to write var bytes")
	}

	return nil
}

func (e *hdPubKeyEncrypter) Encrypt(payload []byte) (string, error) {
	// Uses AES128-GCM with associated data. ECDHE is used for key exchange and ECDSA for authentication.
	// The goal is to be able to send an arbitrary message to a 3rd party or our future selves via
	// an intermediary which has knowledge of public keys for all parties involved.
	//
	// Conceptually, what we do is:
	// 1. Sign the payload using the senders private key so the receiver can check it's authentic
	// The signature also covers the receivers public key to avoid payload reuse by the intermediary
	// 2. Establish an encryption key via ECDH given the receivers pub key
	// 3. Encrypt the payload and signature using AES with a new random nonce
	// 4. Add the metadata the receiver will need to decode the message:
	//   * The derivation path for his pub key
	//   * The ephemeral key used for ECDH
	//   * The version code of this scheme
	// 5. HMAC the encrypted payload and the metadata so the receiver can check it hasn't been tampered
	// 6. Add the nonce to the payload so the receiver can actually decrypt the message.
	// The nonce can't be covered by the HMAC since it's used to generate it.
	// 7. Profit!
	//
	// The implementation actually use an AES128-GCM with is an AEAD, so the encryption and HMAC all happen
	// at the same time.

	signingKey, err := e.senderKey.key.ECPrivKey()
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to extract signing key: %w", err)
	}

	encryptionKey, err := e.receiverKey.key.ECPubKey()
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to extract pub key: %w", err)
	}

	// Sign "payload || encryptionKey" to protect against payload reuse by 3rd parties
	signaturePayload := make([]byte, 0, len(payload)+serializedPublicKeyLength)
	signaturePayload = append(signaturePayload, payload...)
	signaturePayload = append(signaturePayload, encryptionKey.SerializeCompressed()...)
	hash := sha256.Sum256(signaturePayload)
	senderSignature, err := btcec.SignCompact(btcec.S256(), signingKey, hash[:], false)
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to sign payload: %w", err)
	}

	// plaintext is "senderSignature || payload"
	plaintext := bytes.NewBuffer(make([]byte, 0, 2+len(payload)+2+len(senderSignature)))
	err = addVariableBytes(plaintext, senderSignature)
	if err != nil {
		return "", fmt.Errorf("Encrypter: failed to add senderSignature: %w", err)
	}

	err = addVariableBytes(plaintext, payload)
	if err != nil {
		return "", fmt.Errorf("Encrypter: failed to add payload: %w", err)
	}

	pubEph, sharedSecret, err := generateSharedEncryptionSecretForAES(encryptionKey)
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to generate shared encryption key: %w", err)
	}

	blockCipher, err := aes.NewCipher(sharedSecret)
	if err != nil {
		return "", fmt.Errorf("Encrypt: new aes failed: %w", err)
	}

	gcm, err := cipher.NewGCM(blockCipher)
	if err != nil {
		return "", fmt.Errorf("Encrypt: new gcm failed: %w", err)
	}

	nonce := randomBytes(gcm.NonceSize())

	// additionalData is "version || pubEph || receiverKeyPath || nonceLen"
	additionalDataLen := 1 + serializedPublicKeyLength + 2 + len(e.receiverKey.Path) + 2
	result := bytes.NewBuffer(make([]byte, 0, additionalDataLen))
	result.WriteByte(PKEncryptionVersion)
	result.Write(pubEph.SerializeCompressed())

	err = addVariableBytes(result, []byte(e.receiverKey.Path))
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to add receiver path: %w", err)
	}

	nonceLen := uint16(len(nonce))
	err = binary.Write(result, binary.BigEndian, &nonceLen)
	if err != nil {
		return "", fmt.Errorf("Encrypt: failed to add nonce len: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext.Bytes(), result.Bytes())

	// result is "additionalData || nonce || ciphertext"
	n, err := result.Write(nonce)
	if err != nil || n != len(nonce) {
		return "", errors.New("Encrypt: failed to add nonce")
	}

	n, err = result.Write(ciphertext)
	if err != nil || n != len(ciphertext) {
		return "", errors.New("Encrypt: failed to add ciphertext")
	}

	return base58.Encode(result.Bytes()), nil
}

// hdPrivKeyDecrypter holds the keys for validation and decryption of messages using Muun's scheme
type hdPrivKeyDecrypter struct {
	receiverKey *HDPrivateKey

	// senderKey optionally holds the pub key used by sender
	// If the sender is the same as the receiver, set this to nil and set fromSelf to true.
	// If the sender is unknown, set this to nil. If so, the authenticity of the message won't be validated.
	senderKey *PublicKey

	// fromSelf is true if this message is from yourself
	fromSelf bool
}

func extractVariableBytes(reader *bytes.Reader, limit int) ([]byte, error) {
	var len uint16
	err := binary.Read(reader, binary.BigEndian, &len)
	if err != nil || int(len) > limit || int(len) > reader.Len() {
		return nil, errors.New("failed to read byte array len")
	}

	result := make([]byte, len)
	n, err := reader.Read(result)
	if err != nil || n != int(len) {
		return nil, errors.New("failed to extract byte array")
	}

	return result, nil
}

func extractVariableString(reader *bytes.Reader, limit int) (string, error) {
	bytes, err := extractVariableBytes(reader, limit)
	return string(bytes), err
}

func (d *hdPrivKeyDecrypter) Decrypt(payload string) ([]byte, error) {
	// Uses AES128-GCM with associated data. ECDHE is used for key exchange and ECDSA for authentication.
	// See Encrypt further up for an in depth dive into the scheme used

	decoded := base58.Decode(payload)
	reader := bytes.NewReader(decoded)
	version, err := reader.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to read version byte: %w", err)
	}
	if version != PKEncryptionVersion {
		return nil, fmt.Errorf("Decrypt: found key version %v, expected %v",
			version, PKEncryptionVersion)
	}

	rawPubEph := make([]byte, serializedPublicKeyLength)
	n, err := reader.Read(rawPubEph)
	if err != nil || n != serializedPublicKeyLength {
		return nil, errors.New("Decrypt: failed to read pubeph")
	}

	receiverPath, err := extractVariableString(reader, maxDerivationPathLen)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to extract receiver path: %w", err)
	}

	// additionalDataSize is Whatever I've read so far plus two bytes for the nonce len
	additionalDataSize := len(decoded) - reader.Len() + 2

	minCiphertextLen := 2 // an empty sig with no plaintext
	nonce, err := extractVariableBytes(reader, reader.Len()-minCiphertextLen)
	if err != nil || len(nonce) < minNonceLen {
		return nil, errors.New("Decrypt: failed to read nonce")
	}

	// What's left is the ciphertext
	ciphertext := make([]byte, reader.Len())
	_, err = reader.Read(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to read ciphertext: %w", err)
	}

	receiverKey, err := d.receiverKey.DeriveTo(receiverPath)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to derive receiver key to path %v: %w", receiverPath, err)
	}

	encryptionKey, err := receiverKey.key.ECPrivKey()
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to extract encryption key: %w", err)
	}

	var verificationKey *btcec.PublicKey
	if d.fromSelf {
		// Use the derived receiver key if the sender key is not provided
		verificationKey, err = receiverKey.PublicKey().key.ECPubKey()
		if err != nil {
			return nil, fmt.Errorf("Decrypt: failed to extract verification key: %w", err)
		}
	} else if d.senderKey != nil {
		verificationKey = d.senderKey.key
	}

	sharedSecret, err := recoverSharedEncryptionSecretForAES(encryptionKey, rawPubEph)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to recover shared secret: %w", err)
	}

	blockCipher, err := aes.NewCipher(sharedSecret)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: new aes failed: %w", err)
	}

	gcm, err := cipher.NewGCMWithNonceSize(blockCipher, len(nonce))
	if err != nil {
		return nil, fmt.Errorf("Decrypt: new gcm failed: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, decoded[:additionalDataSize])
	if err != nil {
		return nil, fmt.Errorf("Decrypt: AEAD failed: %w", err)
	}

	plaintextReader := bytes.NewReader(plaintext)

	sig, err := extractVariableBytes(plaintextReader, maxSignatureLen)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to read sig: %w", err)
	}

	data, err := extractVariableBytes(plaintextReader, plaintextReader.Len())
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to extract user data: %w", err)
	}

	signatureData := make([]byte, 0, len(sig)+serializedPublicKeyLength)
	signatureData = append(signatureData, data...)
	signatureData = append(signatureData, encryptionKey.PubKey().SerializeCompressed()...)
	hash := sha256.Sum256(signatureData)
	signatureKey, _, err := btcec.RecoverCompact(btcec.S256(), sig, hash[:])
	if err != nil {
		return nil, fmt.Errorf("Decrypt: failed to verify signature: %w", err)
	}
	if verificationKey != nil && !signatureKey.IsEqual(verificationKey) {
		return nil, errors.New("Decrypt: signing key mismatch")
	}

	return data, nil
}

// Assert hdPubKeyEncrypter fulfills Encrypter interface
var _ Encrypter = (*hdPubKeyEncrypter)(nil)

// Assert hdPrivKeyDecrypter fulfills Decrypter interface
var _ Decrypter = (*hdPrivKeyDecrypter)(nil)

// encryptWithPubKey encrypts a message using a pubKey
// It uses ECDHE/AES/CBC leaving padding up to the caller.
func encryptWithPubKey(pubKey *btcec.PublicKey, plaintext []byte) (*btcec.PublicKey, []byte, error) {
	// Use deprecated ECDH for compat
	pubEph, sharedSecret, err := generateSharedEncryptionSecret(pubKey)
	if err != nil {
		return nil, nil, err
	}
	serializedPubkey := pubEph.SerializeCompressed()
	iv := serializedPubkey[len(serializedPubkey)-aes.BlockSize:]

	ciphertext, err := aescbc.EncryptNoPadding(paddedSerializeBigInt(aescbc.KeySize, sharedSecret), iv, plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("encryptWithPubKey: encrypt failed: %w", err)
	}

	return pubEph, ciphertext, nil
}

// generateSharedEncryptionSecret performs a ECDH with pubKey
// Deprecated: this function is unsafe and generateSharedEncryptionSecretForAES should be used
func generateSharedEncryptionSecret(pubKey *btcec.PublicKey) (*btcec.PublicKey, *big.Int, error) {
	privEph, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, fmt.Errorf("generateSharedEncryptionSecretForAES: failed to generate key: %w", err)
	}

	sharedSecret, _ := pubKey.ScalarMult(pubKey.X, pubKey.Y, privEph.D.Bytes())

	return privEph.PubKey(), sharedSecret, nil
}

// generateSharedEncryptionSecret performs a ECDH with pubKey and produces a secret usable with AES
func generateSharedEncryptionSecretForAES(pubKey *btcec.PublicKey) (*btcec.PublicKey, []byte, error) {
	privEph, sharedSecret, err := generateSharedEncryptionSecret(pubKey)
	if err != nil {
		return nil, nil, err
	}

	hash := sha256.Sum256(paddedSerializeBigInt(aescbc.KeySize, sharedSecret))
	return privEph, hash[:], nil
}

// decryptWithPrivKey decrypts a message encrypted to a pubKey using the corresponding privKey
// It uses ECDHE/AES/CBC leaving padding up to the caller.
func decryptWithPrivKey(privKey *btcec.PrivateKey, rawPubEph []byte, ciphertext []byte) ([]byte, error) {
	// Use deprecated ECDH for compat
	sharedSecret, err := recoverSharedEncryptionSecret(privKey, rawPubEph)
	if err != nil {
		return nil, err
	}

	iv := rawPubEph[len(rawPubEph)-aes.BlockSize:]

	plaintext, err := aescbc.DecryptNoPadding(paddedSerializeBigInt(aescbc.KeySize, sharedSecret), iv, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decryptWithPrivKey: failed to decrypt: %w", err)
	}

	return plaintext, nil
}

// recoverSharedEncryptionSecret performs an ECDH to recover the encryption secret meant for privKey from rawPubEph
// Deprecated: this function is unsafe and recoverSharedEncryptionSecretForAES should be used
func recoverSharedEncryptionSecret(privKey *btcec.PrivateKey, rawPubEph []byte) (*big.Int, error) {
	pubEph, err := btcec.ParsePubKey(rawPubEph, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("recoverSharedEncryptionSecretForAES: failed to parse pub eph: %w", err)
	}

	sharedSecret, _ := pubEph.ScalarMult(pubEph.X, pubEph.Y, privKey.D.Bytes())
	return sharedSecret, nil
}

func recoverSharedEncryptionSecretForAES(privKey *btcec.PrivateKey, rawPubEph []byte) ([]byte, error) {
	sharedSecret, err := recoverSharedEncryptionSecret(privKey, rawPubEph)
	if err != nil {
		return nil, err
	}

	hash := sha256.Sum256(paddedSerializeBigInt(aescbc.KeySize, sharedSecret))
	return hash[:], nil
}

func randomBytes(count int) []byte {
	buf := make([]byte, count)
	_, err := rand.Read(buf)
	if err != nil {
		panic("couldn't read random bytes")
	}

	return buf
}