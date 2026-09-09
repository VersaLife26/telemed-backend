package notification

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	userv1 "telemed/internal/pb/user/v1"
)

// Contact is where a person can actually be reached, plus enough identity to
// address them by name.
//
// This service does not own any of it. telemed-user-service does, and the
// canonical user.registered event deliberately does not carry it: a phone
// number is PHI-adjacent and that event fans out to every consumer on the
// platform. So it is fetched, per person, at the moment a notification is
// enqueued.
type Contact struct {
	UserID uuid.UUID
	Name   string
	Phone  string // E.164
	Email  string
	Locale Locale // from the user's language preference, "" when unknown
}

// Directory resolves a user id to their contact details.
//
// It exists as an interface for the usual two reasons -- tests substitute a
// fake, and a future deployment could back it with something other than
// gRPC -- but also for a third: it marks the exact boundary at which this
// service stops being self-contained. Everything else here works from its own
// tables. This one call does not.
type Directory interface {
	Contact(ctx context.Context, userID uuid.UUID) (Contact, error)
}

// ErrContactNotFound means the directory answered, definitively, that it has
// no such user. It is not a system failure: a stale id in an old event is a
// normal thing to encounter. Callers should stop rather than retry.
var ErrContactNotFound = fmt.Errorf("notification: user not found in directory")

// UserServiceDirectory resolves contacts over telemed-user-service's internal
// gRPC surface (proto/user/v1), which is the platform's sanctioned way to read
// another service's data -- never a cross-database join (ADR-004).
//
// The call happens in the event consumer, at enqueue time, not on the delivery
// path: the resolved address is written to notifications.recipient and the
// dispatcher reads it from there. So the "no synchronous cross-service call
// while delivering" property still holds. What changed is that enqueuing is
// now allowed to fail and be redelivered, which is strictly better than
// enqueuing a notification addressed to the empty string.
type UserServiceDirectory struct {
	client  userv1.UserServiceClient
	conn    *grpc.ClientConn
	timeout time.Duration
}

// NewUserServiceDirectory dials user-service. The dial itself is lazy --
// grpc.NewClient does not block -- so a user-service that is down at boot
// delays notifications rather than preventing this service from starting.
// creds is the machine-to-machine credential presented on every call, and is
// a REQUIRED argument rather than an option: user-service authenticates every
// gRPC method with no exemptions, so a client built without one compiles,
// starts, and then fails to resolve a single recipient. Making the client
// impossible to construct without deciding about credentials is the fix.
//
// Pass nil only where the mesh is genuinely unauthenticated (tests).
func NewUserServiceDirectory(addr string, timeout time.Duration, tlsCfg GRPCTLSConfig, creds credentials.PerRPCCredentials) (*UserServiceDirectory, error) {
	if addr == "" {
		return nil, fmt.Errorf("notification: USER_SERVICE_GRPC_ADDR is required to resolve notification recipients")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	transport, err := tlsCfg.credentials()
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	if creds != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(creds))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("notification: dial user-service at %s: %w", addr, err)
	}
	return &UserServiceDirectory{client: userv1.NewUserServiceClient(conn), conn: conn, timeout: timeout}, nil
}

// Close releases the gRPC connection.
func (d *UserServiceDirectory) Close() error {
	if d.conn == nil {
		return nil
	}
	return d.conn.Close()
}

// Contact fetches one user. A NOT_FOUND answer becomes ErrContactNotFound so
// the caller can distinguish "this user does not exist" (stop) from
// "user-service is unreachable" (retry).
func (d *UserServiceDirectory) Contact(ctx context.Context, userID uuid.UUID) (Contact, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	resp, err := d.client.GetUser(ctx, &userv1.GetUserRequest{UserId: userID.String()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Contact{}, fmt.Errorf("%w: %s", ErrContactNotFound, userID)
		}
		return Contact{}, fmt.Errorf("notification: resolve contact %s: %w", userID, err)
	}
	u := resp.GetUser()
	if u == nil {
		return Contact{}, fmt.Errorf("%w: %s", ErrContactNotFound, userID)
	}

	c := Contact{UserID: userID, Name: u.GetName(), Phone: u.GetPhone(), Email: u.GetEmail()}
	if loc := Locale(u.GetLanguage()); ValidLocale(loc) {
		c.Locale = loc
	}
	return c, nil
}

// --- transport --------------------------------------------------------------

// GRPCTLSConfig decides how this client reaches user-service.
//
// The dial used to be an unconditional insecure.NewCredentials(), justified by
// a comment saying the mesh is private. That is true of the single-host
// docker-compose deployment and is a reasonable position; what was not
// reasonable is that it was the ONLY position available. This link carries the
// recipient's phone number and email address -- the very fields the canonical
// user.registered event deliberately does not broadcast, precisely because
// they are PHI-adjacent -- so a deployment that wants them encrypted must be
// able to say so without editing Go.
//
// So: plaintext is still the default, because the current deployment is a
// single host and mandatory TLS would break it. But it is now a DECISION
// rather than the absence of one:
//
//   - Set USER_SERVICE_GRPC_TLS=true and the dial is TLS, with the system
//     trust store, or a private CA via USER_SERVICE_GRPC_CA_FILE.
//   - In production, plaintext requires USER_SERVICE_GRPC_ALLOW_PLAINTEXT=true
//     to be set deliberately. An unset variable must not silently mean
//     "unencrypted phone numbers across the cluster" -- that is exactly the
//     shape of F17, where a documented control became decoration because
//     nobody set it.
//
// mTLS is out of scope here: it needs a certificate authority and a rotation
// story that do not exist yet, and user-service has no gRPC server-side
// credentials to present. Server-authenticated TLS is what one side can
// deliver alone, and it closes passive interception, which is the realistic
// threat from a compromised pod or a mirrored port.
type GRPCTLSConfig struct {
	// Enabled turns on TLS to user-service.
	Enabled bool
	// CAFile is a PEM bundle for a private CA. Empty means the system pool.
	CAFile string
	// ServerName overrides the name verified in the certificate, for the case
	// where the service is reached by an address that is not its DNS name.
	ServerName string
	// AllowPlaintextInProd must be set explicitly for a production deployment
	// to run without TLS.
	AllowPlaintextInProd bool
	// IsProd is the deployment's own answer to config.Base.IsProd.
	IsProd bool
}

func (c GRPCTLSConfig) credentials() (credentials.TransportCredentials, error) {
	if !c.Enabled {
		if c.IsProd && !c.AllowPlaintextInProd {
			return nil, fmt.Errorf("notification: refusing to dial user-service in plaintext in production. " +
				"This link carries recipients' phone numbers and email addresses. Set USER_SERVICE_GRPC_TLS=true, " +
				"or set USER_SERVICE_GRPC_ALLOW_PLAINTEXT=true to record that the network is trusted on purpose")
		}
		return insecure.NewCredentials(), nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("notification: read USER_SERVICE_GRPC_CA_FILE %s: %w", c.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("notification: USER_SERVICE_GRPC_CA_FILE %s contains no usable certificate", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	return credentials.NewTLS(cfg), nil
}
