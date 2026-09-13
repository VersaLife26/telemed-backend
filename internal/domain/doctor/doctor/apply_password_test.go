package doctor

import (
	"testing"
)

func TestValidApplyPassword(t *testing.T) {
	t.Parallel()
	if validApplyPassword("short") {
		t.Fatal("password shorter than 8 accepted")
	}
	if !validApplyPassword("long-enough") {
		t.Fatal("8+ character password rejected")
	}
}

func TestHashApplyPasswordRoundtrip(t *testing.T) {
	t.Parallel()
	hash, err := hashApplyPassword("secure-pass")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || hash == "secure-pass" {
		t.Fatal("expected a bcrypt hash, not plaintext")
	}
}
