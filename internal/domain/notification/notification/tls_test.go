package notification

import (
	"strings"
	"testing"
)

// The gRPC hop to user-service carries the recipient's phone number and email address. The dial used to be an unconditional insecure.NewCredentials(),
// justified by a comment saying the mesh is private -- which is true of the
// single-host deployment and is a reasonable position, but it was the ONLY
// position available.
//
// Plaintext stays the default, because the current deployment is a single host
// and mandatory TLS would break it. What must not happen is production picking
// it up from an unset variable: that is the shape of F17, where a documented
// control became decoration because nobody set it.
func TestTLSConfig_ProductionMustDecideAboutPlaintext(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     GRPCTLSConfig
		wantErr bool
	}{
		{"dev, nothing set: plaintext, as today", GRPCTLSConfig{}, false},
		{"dev with TLS on", GRPCTLSConfig{Enabled: true}, false},
		{"prod with TLS on", GRPCTLSConfig{Enabled: true, IsProd: true}, false},
		{"prod, nothing set: refuses to boot", GRPCTLSConfig{IsProd: true}, true},
		{"prod, plaintext acknowledged in writing", GRPCTLSConfig{IsProd: true, AllowPlaintextInProd: true}, false},
		// The acknowledgement is not a way to skip TLS you asked for.
		{"prod with TLS on, acknowledgement irrelevant", GRPCTLSConfig{Enabled: true, IsProd: true, AllowPlaintextInProd: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			creds, err := tc.cfg.credentials()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%+v was accepted; production must not fall back to unencrypted PII from an unset variable", tc.cfg)
				}
				if !strings.Contains(err.Error(), "USER_SERVICE_GRPC_TLS") {
					t.Fatalf("the error must name the variable that fixes it: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%+v was refused: %v", tc.cfg, err)
			}
			if creds == nil {
				t.Fatal("nil credentials")
			}
			want := "insecure"
			if tc.cfg.Enabled {
				want = "tls"
			}
			if got := creds.Info().SecurityProtocol; got != want {
				t.Fatalf("security protocol = %q, want %q", got, want)
			}
		})
	}
}

// A CA file that is missing or is not a certificate must fail the dial, not
// silently fall back to the system pool -- which would verify against a
// public CA a private service will never present.
func TestTLSConfig_ABadCAFileIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := (GRPCTLSConfig{Enabled: true, CAFile: "/nonexistent/ca.pem"}).credentials(); err == nil {
		t.Fatal("a missing CA file was accepted")
	}

	f := t.TempDir() + "/not-a-cert.pem"
	if err := writeCAFile(f, "this is not a certificate"); err != nil {
		t.Fatal(err)
	}
	if _, err := (GRPCTLSConfig{Enabled: true, CAFile: f}).credentials(); err == nil {
		t.Fatal("a CA file containing no certificate was accepted")
	}
}
