// Package provisioning provides the provisioning job worker for DNSFox v2.
// encryptor.go implements AES-256-GCM payload encryption with HKDF-SHA256
// key derivation so each server gets a unique derived key.
package provisioning

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// deriveKey produces a 32-byte AES-256 key from a master key and server ID
// using HKDF-SHA256. Each server gets a unique derived key so a compromised
// server's key cannot decrypt payloads for other servers.
func deriveKey(masterKey []byte, serverID string) ([]byte, error) {
	info := []byte("dnsfox-provisioning-v1:" + serverID)
	r := hkdf.New(sha256.New, masterKey, nil, info)
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("encryptor: hkdf derive: %w", err)
	}
	return key, nil
}

// EncryptPayload encrypts plaintext using AES-256-GCM with a key derived
// from masterKey + serverID. The returned bytes are: nonce (12 bytes) || ciphertext+tag.
func EncryptPayload(masterKey []byte, serverID string, plaintext []byte) ([]byte, error) {
	key, err := deriveKey(masterKey, serverID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryptor: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryptor: new gcm: %w", err)
	}

	// Generate a random 12-byte nonce — GCM standard nonce size.
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encryptor: generate nonce: %w", err)
	}

	// Seal appends ciphertext+tag after the nonce so the output is self-contained.
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// DecryptPayload decrypts a blob produced by EncryptPayload.
// The input format is: nonce (12 bytes) || ciphertext+tag.
func DecryptPayload(masterKey []byte, serverID string, ciphertext []byte) ([]byte, error) {
	key, err := deriveKey(masterKey, serverID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryptor: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryptor: new gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("encryptor: ciphertext too short")
	}

	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("encryptor: decrypt: %w", err)
	}
	return plaintext, nil
}
