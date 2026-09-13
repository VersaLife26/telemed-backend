// Command server is the entrypoint template every telemed Go service follows.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
//
// telemed-doctor-service additionally runs a gRPC listener (doctor.v1, for
// other services to resolve a doctor without joining across the database
// boundary) and two NATS consumers: one that folds slot.generated /
// slot.booked / slot.released into the local availability projection search
// depends on, and one that reacts to appointment.completed. See
// docs/DESIGN.md for why both exist.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/domain/doctor/doctor"
	"telemed/internal/domain/user/user"
	"telemed/internal/platform/config"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/servicetoken"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-doctor-service"

// serviceConfig extends the shared config.Base with the keys this service
// alone needs. Embedding rather than duplicating fields is what keeps every
// service's HTTP port, database URL, etc. bound the same way.
type serviceConfig struct {
	config.Base `mapstructure:",squash"`

	// BankEncryptionKey is a base64-encoded 32-byte key for the
	// doctor.Encryptor that seals bank payout details before they touch the
	// database. See docs/RUNBOOK.md for rotation.
	BankEncryptionKey string `mapstructure:"bank_encryption_key"`

	// SearchCacheTTL bounds how long a doctor-search result page is served
	// from Redis before the query is re-run.
	SearchCacheTTL time.Duration `mapstructure:"search_cache_ttl"`

	// TrustedProxies is a comma-separated CIDR list whose X-Forwarded-For this
	// service honours. Empty means the private ranges only, never the public
	// internet -- see middleware.DefaultTrustedProxies.
	TrustedProxies string `mapstructure:"trusted_proxies"`

	// SchedulingBaseURL is where scheduling-service listens, e.g.
	// http://scheduling:8083. It is used for exactly one thing: forwarding the
	// `holidays` array on PUT /doctors/me/availability to the service that owns
	// the holidays table (ADR-004 -- doctor-service must not write it).
	//
	// Empty disables forwarding. An availability save carrying leave is then
	// refused with 501 rather than accepted and dropped, because a doctor told
	// their leave saved while patients keep booking them is the failure this
	// whole path exists to prevent. Saves with no leave -- the common case --
	// are unaffected.
	SchedulingBaseURL string `mapstructure:"scheduling_base_url"`

	// SchedulingTimeout bounds the holiday forward. It sits on a doctor's Save
	// button, so it is short by design.
	SchedulingTimeout time.Duration `mapstructure:"scheduling_timeout"`

	// UserServiceURL is the user-service HTTP base used after admin approval
	// to create the doctor login. Empty falls back to the in-process
	// provisioner when both domains share a process.
	UserServiceURL string `mapstructure:"user_service_url"`

	MeshClientID     string `mapstructure:"mesh_client_id"`
	MeshClientSecret string `mapstructure:"mesh_client_secret"`
	MeshTokenURL     string `mapstructure:"mesh_token_url"`
}

// ProxyCIDRs splits the trusted-proxy list, dropping empty entries so that a
// trailing comma is not read as a network.
func (c serviceConfig) ProxyCIDRs() []string {
	raw := strings.TrimSpace(c.TrustedProxies)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// internalRoles are the roles permitted on the internal admin-service-facing
// surface: every human admin role plus the machine-to-machine "service" role
// admin-service itself authenticates as.
var internalRoles = append([]middleware.Role{middleware.RoleService}, middleware.AdminRoles...)

type userDoctorProvisioner interface {
	ProvisionDoctor(context.Context, user.ProvisionDoctorInput) (user.ProvisionDoctorResult, error)
}

type inProcessAccountProvisioner struct {
	inner userDoctorProvisioner
}

func (p inProcessAccountProvisioner) ProvisionDoctor(ctx context.Context, in doctor.DoctorAccount) (doctor.ProvisionResult, error) {
	res, err := p.inner.ProvisionDoctor(ctx, user.ProvisionDoctorInput{
		Email: in.Email, Phone: in.Phone, Name: in.Name, PasswordHash: in.PasswordHash,
	})
	if err != nil {
		return doctor.ProvisionResult{}, classifyProvisionError(err)
	}
	return doctor.ProvisionResult{UserID: res.User.ID, PasswordApplied: res.PasswordApplied}, nil
}

// classifyProvisionError is the in-process twin of the HTTP client's status
// mapping: the same user-domain errors that become a 4xx over the mesh have to
// become ErrAccountConflict here, or the same approval would be reported as
// retryable in the monolith and permanent when the domains are split.
func classifyProvisionError(err error) error {
	switch {
	case errors.Is(err, user.ErrEmailTaken),
		errors.Is(err, user.ErrPhoneTaken),
		errors.Is(err, user.ErrUserSuspended),
		errors.Is(err, user.ErrUserDeleted),
		errors.Is(err, user.ErrInvalidEmail),
		errors.Is(err, user.ErrInvalidPhone),
		errors.Is(err, user.ErrInvalidPassword):
		return fmt.Errorf("%w: %v", doctor.ErrAccountConflict, err)
	default:
		return err
	}
}

func accountProvisionerFromDeps(deps modular.Deps, cfg serviceConfig, log zerolog.Logger) doctor.AccountProvisioner {
	if deps.Registry != nil {
		if v, ok := deps.Registry.Lookup(modular.KeyDoctorAccountProvisioner); ok {
			if svc, ok := v.(userDoctorProvisioner); ok {
				return inProcessAccountProvisioner{inner: svc}
			}
		}
	}
	return buildHTTPAccountProvisioner(cfg, log)
}

func buildHTTPAccountProvisioner(cfg serviceConfig, log zerolog.Logger) doctor.AccountProvisioner {
	if strings.TrimSpace(cfg.UserServiceURL) == "" {
		return nil
	}
	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     cfg.MeshTokenURL,
		ClientID:     cfg.MeshClientID,
		ClientSecret: cfg.MeshClientSecret,
		RequireTLS:   false,
	})
	if err != nil {
		log.Warn().Err(err).Msg("doctor login provisioner unavailable")
		return nil
	}
	return doctor.NewHTTPAccountProvisioner(cfg.UserServiceURL, src)
}
