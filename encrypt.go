// encrypt.go encrypts and decrypts binding credentials at rest in the state
// database, using AES-256-GCM with a key from STATE_ENCRYPTION_KEY (base64
// 32 bytes). If no key is set, credentials are stored in plaintext and the
// broker logs a warning at startup.
//
// The approach mirrors the Cloud Foundry cloud-service-broker: stdlib crypto,
// GCM mode, random nonce prefixed to the ciphertext. Key rotation is handled
// by STATE_ENCRYPTION_KEY_OLD: during rotation the broker decrypts with the
// old key and re-encrypts with the new one on the next write.

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
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

// NewGCMEncryptor creates an encryptor from a 32-byte key.
func NewGCMEncryptor(key [32]byte) *GCMEncryptor {
	return &GCMEncryptor{key: key}
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

// NewEncryptorFromEnv builds the encryptor from environment variables.
// Returns a NoopEncryptor if no key is configured.
func NewEncryptorFromEnv() (Encryptor, error) {
	keyB64 := os.Getenv("STATE_ENCRYPTION_KEY")
	if keyB64 == "" {
		return NoopEncryptor{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("STATE_ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("STATE_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	var keyArray [32]byte
	copy(keyArray[:], key)
	return NewGCMEncryptor(keyArray), nil
}
