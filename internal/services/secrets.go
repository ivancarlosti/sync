package services

import (
	"fmt"

	"github.com/ivancarlosti/sync/internal/crypto"
)

// SecretBox is the single place where the application turns plaintext into the
// AES-256-GCM payload stored in the database and back. Every secret column
// (OAuth tokens, SMTP password, webhook signing secret) goes through it.
type SecretBox struct {
	key []byte
}

// NewSecretBox wraps the 32-byte master key parsed from ENCRYPTION_KEY.
func NewSecretBox(key []byte) *SecretBox { return &SecretBox{key: key} }

// Encrypt seals a plaintext secret.
func (b *SecretBox) Encrypt(plaintext string) (string, error) {
	sealed, err := crypto.Encrypt(b.key, plaintext)
	if err != nil {
		return "", fmt.Errorf("services: encrypting secret: %w", err)
	}
	return sealed, nil
}

// Decrypt opens a stored secret. An empty payload is returned as an empty
// string so optional secrets do not have to be special-cased by every caller.
func (b *SecretBox) Decrypt(payload string) (string, error) {
	if payload == "" {
		return "", nil
	}
	plaintext, err := crypto.Decrypt(b.key, payload)
	if err != nil {
		return "", fmt.Errorf("services: decrypting secret: %w", err)
	}
	return plaintext, nil
}

// Mask renders a stored secret for the API: it never returns the value, only
// whether one is present.
func Mask(present bool) string {
	if present {
		return "********"
	}
	return ""
}
