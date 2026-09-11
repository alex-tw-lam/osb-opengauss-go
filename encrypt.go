// encrypt.go encrypts and decrypts binding credentials at rest in the state
// database, using AES-256-GCM with the key supplied by the configuration
// (base64 32 bytes). Without a key, credentials are stored in plaintext and
// the broker logs a warning at startup. While STATE_ENCRYPTION_KEY_PREVIOUS
// is set, the store re-encrypts old records with the new key at startup.
//
// Same approach as the Cloud Foundry cloud-service-broker: stdlib crypto,
// GCM mode, random nonce prefixed to the ciphertext.

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encryptor encrypts and decrypts byte slices.
type Encryptor interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// NoopEncryptor stores data as-is (for when no key is configured).
type NoopEncryptor struct{}

func (NoopEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (NoopEncryptor) Decrypt(c []byte) ([]byte, error) { return c, nil }

// GCMEncryptor implements AES-256-GCM.
type GCMEncryptor struct {
	key [32]byte
}

func (e *GCMEncryptor) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(e.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (e *GCMEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	gcm, err := e.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (e *GCMEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	gcm, err := e.gcm()
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
}

// NewEncryptor builds the encryptor from a base64 key; an empty key means
// plaintext storage (a NoopEncryptor).
func NewEncryptor(keyB64 string) (Encryptor, error) {
	if keyB64 == "" {
		return NoopEncryptor{}, nil
	}
	key, err := parseKey(keyB64, "STATE_ENCRYPTION_KEY")
	if err != nil {
		return nil, err
	}
	return &GCMEncryptor{key: key}, nil
}

// NewPreviousKeyDecryptor builds a decryptor for the key being retired, so
// RotateBindings can read records written before a rotation.
func NewPreviousKeyDecryptor(keyB64 string) (Encryptor, error) {
	key, err := parseKey(keyB64, "STATE_ENCRYPTION_KEY_PREVIOUS")
	if err != nil {
		return nil, err
	}
	return &GCMEncryptor{key: key}, nil
}

func parseKey(keyB64, envName string) ([32]byte, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%s is not valid base64: %w", envName, err)
	}
	if len(key) != 32 {
		return [32]byte{}, fmt.Errorf("%s must decode to 32 bytes, got %d", envName, len(key))
	}
	var keyArray [32]byte
	copy(keyArray[:], key)
	return keyArray, nil
}
