package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Sealed tokens are "v1:" + base64(nonce || AES-256-GCM ciphertext) under a
// key derived from SESSION_SECRET. The version prefix leaves room to rotate
// the scheme without guessing at old rows.
const tokenCryptVersion = "v1"

var errTokenCryptVersion = errors.New("sealed token: unsupported version")

func tokenCryptKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// SealToken encrypts plaintext for storage on a session row. An empty
// plaintext seals to an empty string so "no token" stays a plain empty column.
func SealToken(secret, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if secret == "" {
		return "", errors.New("sealed token: session secret is empty")
	}
	block, err := aes.NewCipher(tokenCryptKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(tokenCryptVersion))
	return tokenCryptVersion + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// OpenToken reverses SealToken. Tampered or foreign-key ciphertext fails, so
// a rotated SESSION_SECRET simply reads as "no token".
func OpenToken(secret, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	version, payload, ok := strings.Cut(sealed, ":")
	if !ok || version != tokenCryptVersion {
		return "", errTokenCryptVersion
	}
	raw, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("sealed token: %w", err)
	}
	block, err := aes.NewCipher(tokenCryptKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed token: ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(tokenCryptVersion))
	if err != nil {
		return "", fmt.Errorf("sealed token: %w", err)
	}
	return string(plaintext), nil
}
