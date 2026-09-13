package doctor

import (
	"fmt"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	// Match user-service password.go: same cost so hashes are interchangeable
	// when OTP attach copies them onto users.password_hash.
	applyPasswordCost = 12
	minApplyPassword  = 8
	maxApplyPassword  = 72
)

func validApplyPassword(password string) bool {
	if len(password) < minApplyPassword || len(password) > maxApplyPassword {
		return false
	}
	for _, r := range password {
		if unicode.IsSpace(r) && r != ' ' {
			return false
		}
	}
	return true
}

func hashApplyPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), applyPasswordCost)
	if err != nil {
		return "", fmt.Errorf("doctor: hash apply password: %w", err)
	}
	return string(h), nil
}
