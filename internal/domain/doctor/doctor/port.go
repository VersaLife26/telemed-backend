package doctor

import "context"

// Encryptor performs field-level encryption of sensitive doctor data (bank
// payout details). Business code never calls a KMS SDK directly; it depends
// on this interface so the backing implementation -- today a NaCl secretbox
// keyed from config, tomorrow AWS KMS, GCP KMS, or Keycloak-managed secrets
// -- can change without touching service.go. This is the same "50-year
// rule" the platform applies to VideoProvider, PaymentProvider and Storage.
//
// Implementations must guarantee the plaintext never reaches disk or a log
// line; Encrypt/Decrypt are the only places plaintext bank details may ever
// exist in memory.
type Encryptor interface {
	// Encrypt returns opaque ciphertext safe to store in bank_encrypted.
	Encrypt(ctx context.Context, plaintext []byte) (ciphertext string, err error)
	// Decrypt reverses Encrypt. Returns an error if ciphertext is malformed
	// or was not produced by this key.
	Decrypt(ctx context.Context, ciphertext string) (plaintext []byte, err error)
}
