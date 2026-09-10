package config

import "testing"

// TestTestModeEnabled_ProductionOverridesTheFlag is the control that keeps an
// unauthenticated, OTP-revealing surface out of production. The flag defaults
// to true and travels in .env files, so the environment -- not the flag -- has
// to be what decides.
func TestTestModeEnabled_ProductionOverridesTheFlag(t *testing.T) {
	cases := []struct {
		env  string
		flag bool
		want bool
	}{
		{"dev", true, true},
		{"staging", true, true},
		{"dev", false, false},

		// Every spelling IsProd accepts must override, including the ones a
		// naive `env == "prod"` check would miss.
		{"prod", true, false},
		{"production", true, false},
		{"Production", true, false},
		{"PROD", true, false},
		{"live", true, false},
		{"  prod  ", true, false},
	}
	for _, c := range cases {
		got := Base{Env: c.env, TestMode: c.flag}.TestModeEnabled()
		if got != c.want {
			t.Errorf("Base{Env:%q, TestMode:%v}.TestModeEnabled() = %v, want %v",
				c.env, c.flag, got, c.want)
		}
	}
}

// The default must be on, per the deployment model: every non-production
// environment wants the test surface without being configured for it.
func TestTestModeDefaultsOn(t *testing.T) {
	v := New("telemed-test")
	if !v.GetBool("telemed_test_mode") {
		t.Fatal("telemed_test_mode does not default to true")
	}
}
