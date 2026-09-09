package doctor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// SecretboxEncryptor implements Encryptor with NaCl secretbox (XSalsa20 +
// Poly1305): a single 32-byte key, authenticated encryption, no IV-reuse
// footguns because the nonce is generated fresh per call and stored
// alongside the ciphertext. It is the "AES-GCM or NaCl secretbox with a key
// from config" option the build contract calls out as acceptable for this
// pass; swap it for a KMS-backed Encryptor by implementing the same
// interface, no caller changes required.
type SecretboxEncryptor struct {
	key [32]byte
}

var _ Encryptor = (*SecretboxEncryptor)(nil)

// NewSecretboxEncryptor builds an Encryptor from a base64-encoded 32-byte key
// (e.g. from BANK_ENCRYPTION_KEY, generated with `openssl rand -base64 32`).
func NewSecretboxEncryptor(base64Key string) (*SecretboxEncryptor, error) {
	raw, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("doctor: decode encryption key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("doctor: encryption key must decode to 32 bytes, got %d", len(raw))
	}
	var key [32]byte
	copy(key[:], raw)
	return &SecretboxEncryptor{key: key}, nil
}

// Encrypt seals plaintext under a fresh random nonce and returns
// base64(nonce || ciphertext). The nonce need not be secret, only unique per
// message, and a fresh crypto/rand nonce on every call satisfies that.
func (e *SecretboxEncryptor) Encrypt(_ context.Context, plaintext []byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("doctor: generate nonce: %w", err)
	}
	sealed := secretbox.Seal(nonce[:], plaintext, &nonce, &e.key)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
func (e *SecretboxEncryptor) Decrypt(_ context.Context, ciphertext string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("doctor: decode ciphertext: %w", err)
	}
	if len(raw) < 24 {
		return nil, fmt.Errorf("doctor: ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])

	plaintext, ok := secretbox.Open(nil, raw[24:], &nonce, &e.key)
	if !ok {
		return nil, fmt.Errorf("doctor: decrypt failed: authentication mismatch")
	}
	return plaintext, nil
}
