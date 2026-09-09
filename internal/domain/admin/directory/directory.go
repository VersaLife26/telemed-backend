// Package directory resolves a user id to the identity fields the admin
// console displays: name, email, phone.
//
// This service owns none of that. telemed-user-service does, and the
// canonical user.registered event deliberately does not carry it -- a full
// phone number is PHI-adjacent, and that event fans out to every consumer on
// the platform. So the admin projections fetch it, per user, over
// user-service's internal gRPC surface (ADR-004: cross-service reads go over
// gRPC, never a join across database boundaries).
//
// Before this package existed, internal/users and internal/credentialing each
// declared a private payload struct listing full_name/email/phone as fields
// they expected on the event. Nobody sent them. encoding/json left them as
// "", so every row in the admin user search and every doctor in the
// credentialing queue was projected nameless, and nothing logged an error --
// the same failure _shared/INTEGRATION-FIXES.md item 9 documents for
// doctor.approved.
package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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

// ErrNotFound means user-service answered, definitively, that it has no such
// user. A stale id in a replayed event is a normal thing to meet; it is not a
// system failure and retrying will not help.
var ErrNotFound = errors.New("directory: user not found")

// User is the slice of a user record the admin console needs.
type User struct {
	UserID   uuid.UUID
	FullName string
	Email    string
	Phone    string
	Role     string
	Status   string
}

// Client resolves users. It is an interface so projector tests can substitute
// a fake, and so a future deployment could resolve identities from somewhere
// other than gRPC without touching a projector.
type Client interface {
	User(ctx context.Context, userID uuid.UUID) (User, error)
}

// GRPCClient talks to telemed-user-service.
type GRPCClient struct {
	client  userv1.UserServiceClient
	conn    *grpc.ClientConn
	timeout time.Duration
}

// New dials user-service. grpc.NewClient does not block, so a user-service
// that is down at boot delays projections rather than preventing this service
// from starting -- the admin console's own pages keep working, they just
// stop learning about new users until it returns.
// creds is the machine-to-machine credential presented on every call. It is a
// REQUIRED argument rather than an option, because it was the absence of one
// that broke this integration: user-service's gRPC surface authenticates every
// method, and a client with no credential compiles, starts, and then fails on
// every call. Making it impossible to construct this client without deciding
// about credentials is the point.
//
// Pass nil ONLY where the mesh is genuinely unauthenticated -- tests that
// stand up their own server. Production must pass a real source.
func New(addr string, timeout time.Duration, tlsCfg TLSConfig, creds credentials.PerRPCCredentials) (*GRPCClient, error) {
	if addr == "" {
		return nil, fmt.Errorf("directory: USER_SERVICE_GRPC_ADDR is required " +
			"(user names, emails and phones are resolved there; the events do not carry them)")
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
		return nil, fmt.Errorf("directory: dial user-service at %s: %w", addr, err)
	}
	return &GRPCClient{client: userv1.NewUserServiceClient(conn), conn: conn, timeout: timeout}, nil
}

// Close releases the gRPC connection.
// NewInProcess wraps a UserServiceClient that is already resolved, instead of
// dialling one.
//
// cmd/telemed passes user.InProcessClient here when the user domain is loaded
// in this process. Nothing else changes: this type still holds a
// userv1.UserServiceClient and still applies its own timeout, so the mapping
// of NOT_FOUND, the PHI-safe logging and the projection behaviour are the same
// code either way. conn stays nil, and Close is written to tolerate that.
//
// When the user domain is NOT in this process, the composer calls New
// instead and the real gRPC client -- with its required mesh credential -- is
// used exactly as before.
func NewInProcess(client userv1.UserServiceClient, timeout time.Duration) *GRPCClient {
	return &GRPCClient{client: client, timeout: timeout}
}

func (c *GRPCClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// User fetches one user, mapping NOT_FOUND to ErrNotFound so a caller can
// tell "this user does not exist" (stop) from "user-service is unreachable"
// (retry).
func (c *GRPCClient) User(ctx context.Context, userID uuid.UUID) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.client.GetUser(ctx, &userv1.GetUserRequest{UserId: userID.String()})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return User{}, fmt.Errorf("%w: %s", ErrNotFound, userID)
		}
		return User{}, fmt.Errorf("directory: resolve user %s: %w", userID, err)
	}
	u := resp.GetUser()
	if u == nil {
		return User{}, fmt.Errorf("%w: %s", ErrNotFound, userID)
	}
	return User{
		UserID:   userID,
		FullName: u.GetName(),
		Email:    u.GetEmail(),
		Phone:    u.GetPhone(),
		Role:     u.GetRole(),
		Status:   u.GetStatus(),
	}, nil
}

// --- transport --------------------------------------------------------------

// TLSConfig decides how this client reaches user-service.
//
// The dial used to be an unconditional insecure.NewCredentials(), justified by
// a comment saying the mesh is private. That is true of the single-host
// docker-compose deployment and is a reasonable position; what was not
// reasonable is that it was the ONLY position available. The link carries
// names, phone numbers and email addresses -- PII by any definition, and
// PDPA-relevant here -- so a deployment that wants it encrypted must be able
// to say so without editing Go.
//
// So: plaintext is still the default, because the current deployment is a
// single host and mandatory TLS would break it. But it is now a DECISION
// rather than the absence of one:
//
//   - Set USER_SERVICE_GRPC_TLS=true and the dial is TLS, with the system
//     trust store, or a private CA via USER_SERVICE_GRPC_CA_FILE.
//   - In production, plaintext requires USER_SERVICE_GRPC_ALLOW_PLAINTEXT=true
//     to be set deliberately. An unset variable must not silently mean
//     "unencrypted PII across the cluster" -- that is exactly the shape of
//     F17, where a documented control became decoration because nobody set it.
//
// mTLS is out of scope here: it needs a certificate authority and a rotation
// story that do not exist yet, and user-service has no gRPC server-side
// credentials to present. Server-authenticated TLS is what one side can
// deliver alone, and it closes passive interception, which is the realistic
// threat from a compromised pod or a mirrored port.
type TLSConfig struct {
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

func (c TLSConfig) credentials() (credentials.TransportCredentials, error) {
	if !c.Enabled {
		if c.IsProd && !c.AllowPlaintextInProd {
			return nil, fmt.Errorf("directory: refusing to dial user-service in plaintext in production. " +
				"This link carries names, phone numbers and email addresses. Set USER_SERVICE_GRPC_TLS=true, " +
				"or set USER_SERVICE_GRPC_ALLOW_PLAINTEXT=true to record that the network is trusted on purpose")
		}
		return insecure.NewCredentials(), nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("directory: read USER_SERVICE_GRPC_CA_FILE %s: %w", c.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("directory: USER_SERVICE_GRPC_CA_FILE %s contains no usable certificate", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	return credentials.NewTLS(cfg), nil
}
