package user

import (
	"errors"
	"testing"
)

func TestProvisionDoctorValidation(t *testing.T) {
	t.Parallel()
	s := &Service{}
	if _, err := s.ProvisionDoctor(t.Context(), ProvisionDoctorInput{
		Email: "not-an-email", Phone: "+94771234567",
	}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("got %v, want ErrInvalidEmail", err)
	}
	if _, err := s.ProvisionDoctor(t.Context(), ProvisionDoctorInput{
		Email: "ada@example.lk", Phone: "not-a-phone",
	}); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("got %v, want ErrInvalidPhone", err)
	}
	if _, err := s.ProvisionDoctor(t.Context(), ProvisionDoctorInput{
		Email: "ada@example.lk", Phone: "+94771234567", PasswordHash: "plaintext",
	}); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("got %v, want ErrInvalidPassword", err)
	}
}

func TestValidBcryptHash(t *testing.T) {
	t.Parallel()
	if validBcryptHash("") {
		t.Fatal("empty hash must not pass")
	}
	if validBcryptHash("not-a-hash") {
		t.Fatal("plaintext must not pass")
	}
	if !validBcryptHash("$2a$12$abcdefghijklmnopqrstuv") {
		t.Fatal("$2a$ prefix should pass")
	}
	if !validBcryptHash("$2b$12$abcdefghijklmnopqrstuv") {
		t.Fatal("$2b$ prefix should pass")
	}
}
