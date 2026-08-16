package env

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"io"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// EncryptData encrypts plaintext using AES-256-GCM with the provided key
func EncryptData(plaintext string, key []byte) (string, error) {
	// Ensure key is 32 bytes for AES-256
	if len(key) != 32 {
		return "", phelixerr.Newf(phelixerr.CodeEncryption, "invalid key size: expected 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to create cipher", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to create GCM", err)
	}

	// Create nonce (12 bytes is standard for GCM)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to generate nonce", err)
	}

	// Encrypt
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// DecryptData decrypts ciphertext using AES-256-GCM with the provided key
func DecryptData(ciphertext string, key []byte) (string, error) {
	// Ensure key is 32 bytes for AES-256
	if len(key) != 32 {
		return "", phelixerr.Newf(phelixerr.CodeEncryption, "invalid key size: expected 32 bytes, got %d", len(key))
	}

	// Decode hex
	data, err := hex.DecodeString(ciphertext)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to decode hex", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to create cipher", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to create GCM", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", phelixerr.New(phelixerr.CodeEncryption, "ciphertext too short")
	}

	// Extract nonce and actual ciphertext
	nonce, ciphertext_only := data[:nonceSize], data[nonceSize:]

	// Decrypt
	plaintext, err := gcm.Open(nil, nonce, ciphertext_only, nil)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeEncryption, "failed to decrypt", err)
	}

	return string(plaintext), nil
}

// GenerateMasterKey generates a 32-byte AES-256 key
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "failed to generate master key", err)
	}
	return key, nil
}
