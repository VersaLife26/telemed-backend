package main

import "testing"

// A cleartext LiveKit URL used to be accepted anywhere. It is now a boot
// failure outside development, because the clients refuse a non-wss signalling
// URL: the misconfiguration no longer downgrades a consultation to an
// unencrypted one, it fails every join on every device. Better the operator
// sees it than the patient does.
func TestValidateLiveKitURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		env     string
		wantErr bool
	}{
		{"wss in prod", "wss://livekit.yourapp.lk", "prod", false},
		{"wss in dev", "wss://livekit.yourapp.lk", "dev", false},

		// The compose default, which must keep working locally.
		{"ws in dev", "ws://localhost:7880", "dev", false},
		{"ws in test", "ws://localhost:7880", "test", false},

		// The finding.
		{"ws in prod", "ws://livekit.yourapp.lk", "prod", true},
		{"ws in staging", "ws://livekit.yourapp.lk", "staging", true},
		// An env nobody enumerated must fail closed, not fall through.
		{"ws in an unnamed env", "ws://livekit.yourapp.lk", "prod-eu-west", true},

		{"http scheme", "http://livekit.yourapp.lk", "prod", true},
		{"https scheme", "https://livekit.yourapp.lk", "prod", true},
		{"no scheme", "livekit.yourapp.lk", "prod", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLiveKitURL(tc.url, tc.env)
			if tc.wantErr && err == nil {
				t.Errorf("validateLiveKitURL(%q, %q) = nil, want an error", tc.url, tc.env)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateLiveKitURL(%q, %q) = %v, want nil", tc.url, tc.env, err)
			}
		})
	}
}
