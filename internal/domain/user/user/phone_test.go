package user

import (
	"testing"

	"telemed/internal/platform/httpx"
)

// TestNormalizePhone_SriLankanShapes covers every input shape AGENT-BRIEF
// calls out explicitly: "0771234567, 771234567, 94771234567, +94 77 123
// 4567", plus the boundary cases a real OTP send form will actually see
// (spaces, dashes, non-mobile prefixes, landlines, garbage).
//
// The normaliser itself (httpx.NormalizePhone) lives in the shared platform
// package; this test exists in user-service because SendOTP/VerifyOTP is
// where a bug here would actually bite -- a user typing their number one of
// four equally common ways must always resolve to the same account.
func TestNormalizePhone_SriLankanShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"local 0-prefixed", "0771234567", "+94771234567"},
		{"local without leading 0", "771234567", "+94771234567"},
		{"international no plus", "94771234567", "+94771234567"},
		{"international with plus", "+94771234567", "+94771234567"},
		{"international with spaces", "+94 77 123 4567", "+94771234567"},
		{"local with dashes", "077-123-4567", "+94771234567"},
		{"local with spaces", "077 123 4567", "+94771234567"},
		{"parenthesised area-code style", "(0771) 234 567", "+94771234567"},
		{"different mobile operator prefix 70", "0701234567", "+94701234567"},
		{"different mobile operator prefix 75", "0751234567", "+94751234567"},
		{"different mobile operator prefix 76", "0761234567", "+94761234567"},
		{"different mobile operator prefix 78", "0781234567", "+94781234567"},

		{"empty string", "", ""},
		{"too short", "12345", ""},
		{"too long", "0771234567890", ""},
		{"landline (011 prefix, not mobile)", "0112345678", ""},
		{"letters only", "abcdefghij", ""},
		{"missing leading 7 after country code", "0881234567", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := httpx.NormalizePhone(tt.input)
			if got != tt.want {
				t.Errorf("NormalizePhone(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestNormalizePhone_Idempotent asserts that normalising an already-E.164
// number is a no-op, which is what lets the service safely re-normalise a
// value it already stored (e.g. a phone read back from the refresh_tokens
// audit trail) without corrupting it.
func TestNormalizePhone_Idempotent(t *testing.T) {
	inputs := []string{"0771234567", "771234567", "94771234567", "+94771234567"}
	for _, in := range inputs {
		once := httpx.NormalizePhone(in)
		twice := httpx.NormalizePhone(once)
		if once != twice {
			t.Errorf("NormalizePhone(%q) = %q, but NormalizePhone(that) = %q", in, once, twice)
		}
	}
}
