package admin

import (
	"fmt"
	"strings"
	"time"

	"telemed/internal/platform/config"
	"telemed/internal/platform/middleware"
)

// appConfig layers the service-specific keys on top of config.Base -- MinIO
// for the credentialing document viewer and the service-wide admin IP
// allowlist. See internal/adminusers for the additional per-admin scope on
// top of this global list.
type appConfig struct {
	// Admin logins live in Keycloak: it issues the tokens whose realm_access
	// roles authorize every route in this service. These credentials let a
	// super_admin create and re-role colleagues from the console instead of by
	// hand in the Keycloak admin UI. Without them that surface refuses -- see
	// adminusers.UnavailableProvider.
	KeycloakBaseURL           string `mapstructure:"keycloak_base_url"`
	KeycloakRealm             string `mapstructure:"keycloak_realm"`
	KeycloakAdminClientID     string `mapstructure:"keycloak_admin_client_id"`
	KeycloakAdminClientSecret string `mapstructure:"keycloak_admin_client_secret"`

	// The service account this process authenticates AS when it calls another
	// service over gRPC. Distinct from the admin client above: that one manages
	// admin logins, this one is our own identity on the mesh. Its Keycloak
	// service account must hold the "service" realm role.
	MeshClientID     string `mapstructure:"mesh_client_id"`
	MeshClientSecret string `mapstructure:"mesh_client_secret"`
	// MeshTokenURL defaults to the realm token endpoint derived from
	// KeycloakBaseURL and KeycloakRealm; set it only to override.
	MeshTokenURL string `mapstructure:"mesh_token_url"`

	config.Base `mapstructure:",squash"`

	AdminIPAllowlist string `mapstructure:"admin_ip_allowlist"`
	// StorageBackend selects the Storage implementation, matching the record
	// domain's key. Both domains read the same objects, so a deployment that
	// sets one and not the other has two services disagreeing about where the
	// platform's documents live.
	StorageBackend          string `mapstructure:"storage_backend"`
	FilesystemStorageDir    string `mapstructure:"filesystem_storage_dir"`
	FilesystemPresignSecret string `mapstructure:"filesystem_presign_secret"`
	PublicAPIBaseURL        string `mapstructure:"public_api_base_url"`

	MinIOEndpoint         string        `mapstructure:"minio_endpoint"`
	MinIOAccessKey        string        `mapstructure:"minio_access_key"`
	MinIOSecretKey        string        `mapstructure:"minio_secret_key"`
	MinIOUseSSL           bool          `mapstructure:"minio_use_ssl"`
	MinIODoctorDocsBucket string        `mapstructure:"minio_doctor_docs_bucket"`
	DocPresignTTL         time.Duration `mapstructure:"doc_presign_ttl"`

	// UserServiceGRPCAddr is telemed-user-service's internal gRPC endpoint.
	// The user and credentialing projections resolve names, emails and phones
	// there: the canonical user.registered and doctor.registered payloads
	// deliberately do not carry them, because a full phone number is
	// PHI-adjacent and those events fan out to every consumer.
	UserServiceGRPCAddr      string `mapstructure:"user_service_grpc_addr"`
	UserLookupTimeoutSeconds int    `mapstructure:"user_lookup_timeout_seconds"`

	// The gRPC hop to user-service carries names, phone numbers and email
	// addresses. See directory.TLSConfig for why plaintext is still the
	// default and why production has to say so out loud.
	UserServiceGRPCTLS            bool   `mapstructure:"user_service_grpc_tls"`
	UserServiceGRPCCAFile         string `mapstructure:"user_service_grpc_ca_file"`
	UserServiceGRPCServerName     string `mapstructure:"user_service_grpc_server_name"`
	UserServiceGRPCAllowPlaintext bool   `mapstructure:"user_service_grpc_allow_plaintext"`

	// DoctorServiceURL is the doctor-service HTTP base used to approve/reject
	// public applications (SoR). Empty keeps the legacy event-only path.
	DoctorServiceURL string `mapstructure:"doctor_service_url"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// this service believes. Requests from anywhere else have their client
	// address taken from the TCP peer, which no header can forge.
	//
	// On THIS service that is a security boundary, not a nicety: the admin
	// IP allowlist below, the per-admin CIDR scope in internal/adminusers, and
	// every row in the append-only audit log all key on the resolved address.
	// chi's deprecated middleware.RealIP, which this service used to install,
	// took it from a client-supplied header (GHSA-3fxj-6jh8-hvhx), so a single
	// forged X-Forwarded-For walked straight through the allowlist.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// Validate is the boot-time gate. It runs config.Base.Validate first, then the
// checks that are specific to an ADMIN service -- the one surface on this
// platform where "not configured" must not mean "open".
//
// SECURITY REVIEW F17. middleware.IPAllowlist treats an empty list as "not
// configured" and calls next.ServeHTTP: allow everyone. Its comment claimed
// "in prod the deployment must set it; the readiness check asserts that
// separately", and no such check existed anywhere -- not in config.Base, not in
// a health check, and ADMIN_IP_ALLOWLIST was not even present in
// telemed-infra's admin-service ConfigMap. So an unset environment variable
// silently converted a documented control into decoration, and the failure was
// invisible: the service booted, served, and logged nothing unusual while the
// doctor-credentialing queue and the audit log sat open to any address that
// could reach the pod.
//
// Failing open is a defensible default for a rate limiter. It is the wrong
// default for the admin surface, where the blast radius of "everyone" is every
// patient record the console can reach. So: in prod, no allowlist, no boot.
// The gateway made the same change at its edge (F17, fixed there); this is the
// same rule applied where the decision is actually made, so a request that
// reaches this service inside the mesh cannot bypass it.
//
// KEYCLOAK_ISSUER is checked for the same reason. Keycloak is the ONLY issuer
// permitted to assert an admin role (ADR-010, and F1's issuer binding). If it
// is unset, Base.IssuerKeys() simply omits it, and this service boots trusting
// user-service alone -- a phone-OTP token issuer -- for a surface that exists
// on the assumption of SAML SSO and enforced 2FA.
func (c appConfig) Validate() error {
	if err := c.Base.Validate(); err != nil {
		return err
	}

	if !c.IsProd() {
		// Dev and staging keep the convenience. Warn-free by design: the
		// prod check below is the control, and a warning nobody reads on
		// every local boot trains people to ignore the log.
		return validateAllowlistSyntax(c.AdminIPAllowlist)
	}

	var problems []string
	// splitCSV, not strings.TrimSpace: main.go builds the live allowlist with
	// splitCSV, so that is the function that decides whether the list is
	// empty. Checking TrimSpace instead would pass ADMIN_IP_ALLOWLIST=" , , "
	// -- non-empty as a string, zero entries as a list -- straight through to
	// an open admin surface. The check has to ask the same question the
	// runtime asks.
	if len(splitCSV(c.AdminIPAllowlist)) == 0 {
		problems = append(problems, "ADMIN_IP_ALLOWLIST has no usable entries, which middleware.IPAllowlist treats as "+
			"\"allow every address\"; set the office and VPN CIDRs")
	}
	if strings.TrimSpace(c.KeycloakIssuer) == "" || strings.TrimSpace(c.KeycloakJWKSURL) == "" {
		problems = append(problems, "KEYCLOAK_ISSUER and KEYCLOAK_JWKS_URL are required: Keycloak is the only "+
			"issuer allowed to assert an admin role (ADR-010)")
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: refusing to start in %s: %s", c.Env, strings.Join(problems, "; "))
	}

	return validateAllowlistSyntax(c.AdminIPAllowlist)
}

// validateAllowlistSyntax rejects an allowlist that parses to nothing useful.
//
// This matters more than it looks. middleware.IPAllowlist logs and SKIPS a
// malformed entry, so "10.0.0.0/8,192.168.0.0.0/16" quietly becomes a
// one-entry list, and "10.0.0/8,192.168.1/16" -- two typos -- becomes an
// EMPTY one, which is the fail-open case again, arrived at from a config that
// looks configured. Catching it at boot turns a silent downgrade into a
// startup error naming the entry.
func validateAllowlistSyntax(raw string) error {
	entries := splitCSV(raw)
	if len(entries) == 0 {
		return nil
	}
	_, malformed := middleware.NewTrustedProxies(entries)
	if len(malformed) > 0 {
		return fmt.Errorf("config: ADMIN_IP_ALLOWLIST contains %d unparseable entr%s: %s",
			len(malformed), plural(len(malformed), "y", "ies"), strings.Join(malformed, ", "))
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
