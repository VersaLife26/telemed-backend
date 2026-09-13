package doctor

import (
	"fmt"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	// Match user-service: bcrypt cost 12, 8–72 characters (bcrypt's limit).
	applyPasswordCost   = 12
	minApplyPasswordLen = 8
	maxApplyPasswordLen = 72
)

func validApplyPassword(password string) bool {
	if len(password) < minApplyPasswordLen || len(password) > maxApplyPasswordLen {
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
	if !validApplyPassword(password) {
		return "", ErrInvalidPassword
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), applyPasswordCost)
	if err != nil {
		return "", fmt.Errorf("doctor: hash apply password: %w", err)
	}
	return string(h), nil
}
