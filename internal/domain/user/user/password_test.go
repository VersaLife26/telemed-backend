package user

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail("  Ada@Example.LK "); got != "ada@example.lk" {
		t.Fatalf("NormalizeEmail = %q", got)
	}
}

func TestValidEmail(t *testing.T) {
	ok := []string{"ada@example.lk", "a.b+c@mail.co.uk"}
	bad := []string{"", "nope", "ada@", "@x.lk", "ada example@x.lk", "a@b"}
	for _, e := range ok {
		if !validEmail(e) {
			t.Errorf("validEmail(%q) = false, want true", e)
		}
	}
	for _, e := range bad {
		if validEmail(e) {
			t.Errorf("validEmail(%q) = true, want false", e)
		}
	}
}

func TestHashPasswordRoundtrip(t *testing.T) {
	const pw = "correct-horse"
	hash, err := hashPassword(pw)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !passwordMatches(hash, pw) {
		t.Fatal("stored hash did not match the password that produced it")
	}
	if passwordMatches(hash, "wrong-password") {
		t.Fatal("hash matched a different password")
	}
	if consumePasswordCheck("", pw) {
		t.Fatal("empty hash must not authenticate")
	}
}

func TestValidPasswordLength(t *testing.T) {
	if validPassword("short") {
		t.Fatal("password shorter than 8 was accepted")
	}
	if !validPassword("long-enough") {
		t.Fatal("8+ character password was rejected")
	}
	if validPassword(strings.Repeat("a", maxPasswordLen+1)) {
		t.Fatal("password longer than bcrypt's 72-byte limit was accepted")
	}
}

func TestRegisterEmailValidation(t *testing.T) {
	s := &Service{}
	if _, err := s.RegisterEmail(t.Context(), "not-an-email", "long-enough", "", ""); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("got %v, want ErrInvalidEmail", err)
	}
	if _, err := s.RegisterEmail(t.Context(), "ada@example.lk", "short", "", ""); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("got %v, want ErrInvalidPassword", err)
	}
}

func TestLoginGoogleDisabled(t *testing.T) {
	s := &Service{}
	if _, err := s.LoginGoogle(t.Context(), "token", "", true); !errors.Is(err, ErrGoogleDisabled) {
		t.Fatalf("got %v, want ErrGoogleDisabled", err)
	}
}
