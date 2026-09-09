package scheduling

import "testing"

// TestReflectionAllowed pins F5's second half: "Drop reflection.Register outside
// dev."
//
// The interesting cases are the ones a denylist would get wrong. Reverting
// reflectionAllowed to `return !cfg.IsProd()` flips staging, prod-eu-west and
// the empty string to true, and this test fails on all three.
func TestReflectionAllowed(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"dev", true},
		{"development", true},
		{"local", true},
		{"test", true},
		{"DEV", true},
		{" dev ", true},

		{"prod", false},
		{"production", false},
		{"staging", false},
		{"stage", false},
		{"prod-eu-west", false},
		{"", false},
		{"whatever-somebody-typed", false},
	} {
		if got := reflectionAllowed(tc.env); got != tc.want {
			t.Errorf("reflectionAllowed(%q) = %v, want %v", tc.env, got, tc.want)
		}
	}
}
