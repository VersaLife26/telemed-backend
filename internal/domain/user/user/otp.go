package user

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"golang.org/x/crypto/bcrypt"
)

// otpDigits is the length of a generated code. Six digits is the platform
// standard (SDD section 8.1, 33.1).
const otpDigits = 6

// otpMax is the exclusive upper bound for a 6-digit code (000000-999999).
var otpMax = big.NewInt(1000000)

// otpBcryptCost trades a little verify-time CPU for resistance to an attacker
// who obtains the Redis dump. bcrypt's built-in salt also means two identical
// codes never hash identically, which stops an operator from ever eyeballing
// a repeated code across users.
const otpBcryptCost = bcrypt.DefaultCost

// GenerateOTP returns a uniformly random 6-digit code as a zero-padded
// string, e.g. "004821".
//
// It deliberately does not use math/rand (predictable, not for security) and
// does not compute `randomByte % 10` per digit (biased: 256 is not a multiple
// of 10, so digits 0-5 are ever so slightly more likely than 6-9 -- over
// millions of OTPs that bias is a measurable statistical fingerprint an
// attacker can exploit to narrow the search space). crypto/rand.Int performs
// rejection sampling internally, so every value in [0, otpMax) is exactly
// equally likely.
func GenerateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, otpMax)
	if err != nil {
		return "", fmt.Errorf("user: generate otp: %w", err)
	}
	return fmt.Sprintf("%0*d", otpDigits, n.Int64()), nil
}

// HashOTP bcrypt-hashes a code for storage. Only the hash is ever persisted,
// in Redis or anywhere else.
func HashOTP(code string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(code), otpBcryptCost)
	if err != nil {
		return "", fmt.Errorf("user: hash otp: %w", err)
	}
	return string(hash), nil
}

// VerifyOTPHash reports whether code matches hash. bcrypt.CompareHashAndPassword
// compares in constant time with respect to the candidate password, so a
// timing side-channel cannot be used to recover the code digit by digit.
func VerifyOTPHash(hash, code string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(code)) == nil
}
