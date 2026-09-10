package config

import "testing"

// AdminTokenIssuer decides which issuer may assert an administrator role.
// Getting the precedence wrong is not a config nit: a deployment that has
// moved to a new identity provider but still carries a stale KEYCLOAK_ISSUER
// would keep trusting the old one for admin roles, and nothing at runtime
// would say so.
func TestAdminTokenIssuerPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		admin    string
		keycloak string
		want     string
	}{
		{"admin issuer wins", "https://team.cloudflareaccess.com", "http://kc/realms/telemedicine", "https://team.cloudflareaccess.com"},
		{"keycloak is the fallback", "", "http://kc/realms/telemedicine", "http://kc/realms/telemedicine"},
		{"whitespace is not an issuer", "   ", "http://kc/realms/telemedicine", "http://kc/realms/telemedicine"},
		{"neither configured", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Base{AdminIssuer: tt.admin, KeycloakIssuer: tt.keycloak}
			if got := b.AdminTokenIssuer(); got != tt.want {
				t.Errorf("AdminTokenIssuer() = %q, want %q", got, tt.want)
			}
		})
	}
}

// IssuerKeys binds each issuer to the one key set allowed to sign for it.
// A third issuer must not disturb that binding.
func TestIssuerKeysBindsTheAdminIssuerToItsOwnKeys(t *testing.T) {
	b := Base{
		UserIssuer: "telemed-user-service", UserJWKSURL: "http://user/.well-known/jwks.json",
		AdminIssuer: "https://team.cloudflareaccess.com", AdminJWKSURL: "https://team.cloudflareaccess.com/cdn-cgi/access/certs",
	}
	keys := b.IssuerKeys()
	if got := keys["telemed-user-service"]; got != "http://user/.well-known/jwks.json" {
		t.Errorf("user issuer bound to %q", got)
	}
	if got := keys["https://team.cloudflareaccess.com"]; got != "https://team.cloudflareaccess.com/cdn-cgi/access/certs" {
		t.Errorf("admin issuer bound to %q", got)
	}
	if len(keys) != 2 {
		t.Errorf("IssuerKeys() = %v, want exactly the two configured issuers", keys)
	}

	// An issuer with no key set must not appear at all: an entry mapping an
	// issuer to "" would be an issuer this platform claims to trust and has
	// no way to verify.
	partial := Base{AdminIssuer: "https://team.cloudflareaccess.com"}
	if len(partial.IssuerKeys()) != 0 {
		t.Errorf("IssuerKeys() = %v, want none when the JWKS URL is unset", partial.IssuerKeys())
	}
}
