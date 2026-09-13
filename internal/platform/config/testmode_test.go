package config

import "testing"

// The flag alone decides, in every environment.
func TestTestModeEnabled_FollowsTheFlag(t *testing.T) {
	cases := []struct {
		env  string
		flag bool
		want bool
	}{
		{"dev", true, true},
		{"dev", false, false},
		{"production", true, true},
		{"production", false, false},
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
