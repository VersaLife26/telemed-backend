package user

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	// passwordCost is a deliberate security parameter, not a deployment
	// knob. 12 is slow enough on this hardware to make offline guessing of
	// a stolen hash expensive, and fast enough that a login still feels
	// instantaneous. Changing it is a code review.
	passwordCost = 12

	minPasswordLen = 8
	// bcrypt silently truncates after 72 bytes. Rejecting longer passwords
	// is clearer than storing a hash of a prefix the user did not mean.
	maxPasswordLen = 72
	maxEmailLen    = 254
)

var (
	dummyHashOnce sync.Once
	dummyHash     []byte
)

func dummyPasswordHash() []byte {
	dummyHashOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("not-a-real-password"), passwordCost)
		if err != nil {
			// A failure here is a broken crypto/rand, not a user error.
			// Subsequent compares against a nil dummy still fail closed.
			return
		}
		dummyHash = h
	})
	return dummyHash
}

// NormalizeEmail lowercases and trims. Stored emails are always in this form
// so "Ada@X.lk" and "ada@x.lk" are the same account.
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validEmail(email string) bool {
	if email == "" || len(email) > maxEmailLen {
		return false
	}
	at := strings.IndexByte(email, '@')
	if at < 1 || at == len(email)-1 {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	domain := email[at+1:]
	return strings.Contains(domain, ".")
}

func validPassword(password string) bool {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return false
	}
	for _, r := range password {
		if unicode.IsSpace(r) && r != ' ' {
			return false
		}
	}
	return true
}

func hashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func passwordMatches(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// consumePasswordCheck always runs a bcrypt compare so a missing account is
// not cheaper than a wrong password (a timing tell for email enumeration).
func consumePasswordCheck(hash, password string) bool {
	if hash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash(), []byte(password))
		return false
	}
	return passwordMatches(hash, password)
}
