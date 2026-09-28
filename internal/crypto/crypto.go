// Package crypto holds every symmetric primitive used by Sync:
//
//   - AES-256-GCM encryption of stored secrets (OAuth access/refresh tokens,
//     SMTP passwords, webhook signing secrets). Ciphertexts are versioned with
//     the "v1:" prefix so a future key rotation or algorithm change can be
//     detected and migrated without corrupting data.
//   - HMAC-SHA256 signing for the stateless session cookie and for the OAuth
//     `state` values.
//   - PKCE (RFC 7636) helpers for Google/Microsoft/Keycloak authorization code
//     flows.
//
// The package uses the standard library only, which keeps it auditable and
// trivially unit-testable.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CipherPrefix marks every value produced by Encrypt. Decrypt refuses values
// without it, so a plaintext secret accidentally stored in the database is
// never returned as if it were valid ciphertext.
const CipherPrefix = "v1:"

// ErrNotEncrypted is returned by Decrypt when the input is not a Sync payload.
var ErrNotEncrypted = errors.New("value is not a Sync encrypted payload")

// ParseKey normalises ENCRYPTION_KEY into the 32 raw bytes required by
// AES-256. Accepted notations (all equal to 32 bytes of entropy):
//
//	base64 standard / raw / url encoded  → 44 or 43 characters
//	hexadecimal                          → 64 characters
//	raw ASCII                            → exactly 32 characters
func ParseKey(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, errors.New("ENCRYPTION_KEY is empty; generate one with `openssl rand -base64 32`")
	}
	if len(value) == 32 {
		return []byte(value), nil
	}
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		decoded, err := decode(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf(
		"ENCRYPTION_KEY has an unsupported format (%d characters): use 32 raw characters, a 32-byte base64 string or 64 hex characters",
		len(value))
}

// DeriveKey returns a purpose-bound sub-key of the master key. Using one
// sub-key per usage (session cookie, state signing, ...) keeps the master key
// out of every cryptographic context.
func DeriveKey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("sync/" + purpose))
	return mac.Sum(nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("AES-256 requires a 32-byte key, got %d bytes", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating AES-GCM: %w", err)
	}
	return gcm, nil
}

// Encrypt seals plaintext with AES-256-GCM and returns "v1:" followed by the
// base64url encoding of nonce||ciphertext. Encrypting the empty string yields a
// valid ciphertext, so empty secrets round-trip correctly.
func Encrypt(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return CipherPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt opens a payload produced by Encrypt.
func Decrypt(key []byte, payload string) (string, error) {
	if !strings.HasPrefix(payload, CipherPrefix) {
		return "", ErrNotEncrypted
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(payload, CipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decoding ciphertext: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext is shorter than the GCM nonce")
	}
	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypting payload (wrong ENCRYPTION_KEY?): %w", err)
	}
	return string(plaintext), nil
}

// IsEncrypted reports whether the value carries the Sync ciphertext prefix.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, CipherPrefix)
}

// RandomBytes returns n cryptographically secure random bytes.
func RandomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("reading entropy: %w", err)
	}
	return buf, nil
}

// RandomToken returns n random bytes encoded as base64url without padding. It
// is used for opaque state values, IDs and secrets shown to the operator.
func RandomToken(n int) (string, error) {
	buf, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Sign returns the base64url HMAC-SHA256 signature of message.
func Sign(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify recomputes the HMAC of message and compares it with signature in
// constant time.
func Verify(key []byte, message, signature string) bool {
	expected := Sign(key, message)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// PKCEPair generates a code verifier (43 characters, RFC 7636 §4.1) and its
// S256 code challenge.
func PKCEPair() (verifier string, challenge string, err error) {
	verifier, err = RandomToken(32)
	if err != nil {
		return "", "", err
	}
	return verifier, CodeChallengeS256(verifier), nil
}

// CodeChallengeS256 derives the S256 PKCE challenge of a verifier.
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
