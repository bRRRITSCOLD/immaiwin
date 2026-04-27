package workflow

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// EncryptValue encrypts plaintext using AES-256-GCM.
// Returns base64-encoded nonce+ciphertext.
func EncryptValue(plaintext string, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptValue decodes base64 then decrypts AES-256-GCM (nonce prepended).
func DecryptValue(ciphertext string, key []byte) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// EncryptConfigMap encrypts every value in the map. Returns new map.
func EncryptConfigMap(cfg map[string]string, key []byte) (map[string]string, error) {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		enc, err := EncryptValue(v, key)
		if err != nil {
			return nil, fmt.Errorf("encrypt key %q: %w", k, err)
		}
		out[k] = enc
	}
	return out, nil
}

// DecryptConfigMap decrypts every value in the map. Returns new map.
func DecryptConfigMap(cfg map[string]string, key []byte) (map[string]string, error) {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		dec, err := DecryptValue(v, key)
		if err != nil {
			return nil, fmt.Errorf("decrypt key %q: %w", k, err)
		}
		out[k] = dec
	}
	return out, nil
}
