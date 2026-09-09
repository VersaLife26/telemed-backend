package user

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/middleware"
)

// These tests drive a REAL gRPC server over a REAL TCP connection, built from
// GRPCServerOptions -- the same function cmd/server/main.go passes to
// grpc.NewServer. A test that assembled its own interceptor chain would assert
// something true about the test and nothing about :9091.
//
// The finding they exist for: this server was a bare grpc.NewServer() with no
// authentication of any kind. GetUsersBatch returns name, phone, email, role
// and status for an arbitrary list of ids, so anything that could open a TCP
// connection to the port could dump the platform's entire patient directory.
//
// Every case therefore asserts two things, not one: the status code the caller
// sees, AND whether the handler was reached. A 401 that still ran the query
// would have leaked the data already.

// spyUserServer records whether a handler was reached. The gRPC surface is a
// PII directory; "the caller got an error" is not the same claim as "the
// database was never read".
type spyUserServer struct {
	userv1.UnimplementedUserServiceServer
	calls atomic.Int64
	// seen is the principal the interceptor put on the context, so a test can
	// assert the handler reads the CALLER's verified identity rather than a
	// field in the request.
	seen atomic.Value // middleware.Principal
}

func (s *spyUserServer) GetUsersBatch(ctx context.Context, _ *userv1.GetUsersBatchRequest) (*userv1.GetUsersBatchResponse, error) {
	s.calls.Add(1)
	if p, ok := middleware.PrincipalFrom(ctx); ok {
		s.seen.Store(p)
	}
	return &userv1.GetUsersBatchResponse{}, nil
}

const (
	testGRPCIssuer   = "test-service-issuer"
	testGRPCAudience = "telemed-api"
)

// newGRPCTestIssuer returns a TokenIssuer over a fresh ephemeral RSA key, plus a
// live JWKS endpoint serving its public half -- exactly the shape a real
// deployment presents to middleware.Authenticator.
func newGRPCTestIssuer(t *testing.T) (issuer *TokenIssuer, jwksURL string) {
	t.Helper()

	pem, err := GenerateEphemeralKeyPEM()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss, err := NewTokenIssuer(pem, "grpc-test-1", testGRPCIssuer, testGRPCAudience)
	if err != nil {
		t.Fatalf("new token issuer: %v", err)
	}

	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(iss.PublicJWKS())
	}))
	t.Cleanup(jwks.Close)

	return iss, jwks.URL
}

// tokenFor mints a genuine, correctly signed token carrying exactly one role.
// Nothing here is forged: the point of every failing case below is that a
// perfectly valid token still does not open this door.
func tokenFor(t *testing.T, iss *TokenIssuer, role Role) string {
	t.Helper()
	tok, _, err := iss.IssueAccessToken(User{ID: uuid.New(), Phone: "+94771234567", Role: role, Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("issue token for role %q: %v", role, err)
	}
	return tok
}

// startGRPC stands the real server up on a real port and returns a client.
func startGRPC(t *testing.T, auth *middleware.Authenticator, spy *spyUserServer) userv1.UserServiceClient {
	t.Helper()

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(GRPCServerOptions(auth)...)
	userv1.RegisterUserServiceServer(srv, spy)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return userv1.NewUserServiceClient(conn)
}

func callWith(ctx context.Context, client userv1.UserServiceClient, md ...string) error {
	if len(md) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, md...)
	}
	_, err := client.GetUsersBatch(ctx, &userv1.GetUsersBatchRequest{UserIds: []string{uuid.NewString()}})
	return err
}

func TestGRPC_UnauthenticatedCallerCannotDumpTheUserDirectory(t *testing.T) {
	t.Parallel()

	iss, jwksURL := newGRPCTestIssuer(t)
	auth, err := middleware.NewAuthenticatorFrom(context.Background(), middleware.AuthConfig{
		IssuerKeys: map[string]string{testGRPCIssuer: jwksURL},
		Audience:   testGRPCAudience,
	})
	if err != nil {
		t.Fatalf("authenticator: %v", err)
	}

	spy := &spyUserServer{}
	client := startGRPC(t, auth, spy)

	cases := []struct {
		name string
		md   []string
		want codes.Code
	}{
		{
			name: "no metadata at all -- the exploit as reported",
			want: codes.Unauthenticated,
		},
		{
			name: "no authorization key",
			md:   []string{"x-telemed-user-id", uuid.NewString()},
			want: codes.Unauthenticated,
		},
		{
			name: "bare token with no scheme",
			md:   []string{"authorization", tokenFor(t, iss, "service")},
			want: codes.Unauthenticated,
		},
		{
			name: "wrong scheme",
			md:   []string{"authorization", "Basic " + tokenFor(t, iss, "service")},
			want: codes.Unauthenticated,
		},
		{
			name: "Bearer with an empty token",
			md:   []string{"authorization", "Bearer "},
			want: codes.Unauthenticated,
		},
		{
			name: "a syntactically valid but unsigned token",
			md:   []string{"authorization", "Bearer eyJhbGciOiJub25lIn0.eyJzdWIiOiJhIn0."},
			want: codes.Unauthenticated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := spy.calls.Load()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := callWith(ctx, client, tc.md...)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("status = %v (%v), want %v", got, err, tc.want)
			}
			if after := spy.calls.Load(); after != before {
				t.Fatalf("the handler RAN for an unauthenticated caller: the directory was read before the error was returned")
			}
		})
	}
}

func TestGRPC_HumanTokensAreNotKeysToTheInternalMesh(t *testing.T) {
	t.Parallel()

	iss, jwksURL := newGRPCTestIssuer(t)
	auth, err := middleware.NewAuthenticatorFrom(context.Background(), middleware.AuthConfig{
		IssuerKeys: map[string]string{testGRPCIssuer: jwksURL},
		Audience:   testGRPCAudience,
	})
	if err != nil {
		t.Fatalf("authenticator: %v", err)
	}

	spy := &spyUserServer{}
	client := startGRPC(t, auth, spy)

	// Every role the platform can put in a token except "service". A genuine,
	// unexpired, correctly signed patient token is the credential an attacker
	// is most likely to have -- they can mint one themselves with a phone and
	// an OTP.
	for _, role := range []Role{RolePatient, RoleDoctor, RoleAdmin, RoleSuperAdmin, RoleOps, RoleFinance, RoleSupport} {
		t.Run(string(role), func(t *testing.T) {
			before := spy.calls.Load()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := callWith(ctx, client, "authorization", "Bearer "+tokenFor(t, iss, role))
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Fatalf("a genuine %q token got %v (%v), want PermissionDenied", role, got, err)
			}
			if after := spy.calls.Load(); after != before {
				t.Fatalf("the handler RAN for a %q token", role)
			}
		})
	}
}

func TestGRPC_ServiceTokenIsAdmittedAndItsIdentityReachesTheHandler(t *testing.T) {
	t.Parallel()

	iss, jwksURL := newGRPCTestIssuer(t)
	auth, err := middleware.NewAuthenticatorFrom(context.Background(), middleware.AuthConfig{
		IssuerKeys: map[string]string{testGRPCIssuer: jwksURL},
		Audience:   testGRPCAudience,
	})
	if err != nil {
		t.Fatalf("authenticator: %v", err)
	}

	spy := &spyUserServer{}
	client := startGRPC(t, auth, spy)

	callerID := uuid.New()
	tok, _, err := iss.IssueAccessToken(User{ID: callerID, Phone: "+94770000000", Role: "service", Language: LanguageEnglish})
	if err != nil {
		t.Fatalf("issue service token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := callWith(ctx, client, "authorization", "Bearer "+tok); err != nil {
		t.Fatalf("a legitimate service token was refused: %v", err)
	}
	if spy.calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 -- the legitimate path must still work", spy.calls.Load())
	}

	// The interceptor's other half: the handler sees a verified principal, so
	// an actor identity is available from the CALLER rather than from a field
	// in the request body.
	p, ok := spy.seen.Load().(middleware.Principal)
	if !ok {
		t.Fatal("no principal on the handler's context: a handler has no way to know who is calling")
	}
	if p.UserID != callerID {
		t.Fatalf("principal.UserID = %s, want %s", p.UserID, callerID)
	}
	if !p.HasRole(middleware.RoleService) {
		t.Fatalf("principal roles = %v, want the service role", p.Roles)
	}
	if p.Issuer != testGRPCIssuer {
		t.Fatalf("principal.Issuer = %q, want %q -- the issuer must be a verified fact", p.Issuer, testGRPCIssuer)
	}
}

func TestGRPC_MisconfiguredAuthenticationRefusesEveryCall(t *testing.T) {
	t.Parallel()

	// A deployment whose JWKS could not be loaded. The tempting behaviour is
	// to serve traffic anyway -- an HTTP rate limiter fails open for exactly
	// that reason. An authorisation check on a PII directory must not.
	spy := &spyUserServer{}
	client := startGRPC(t, nil, spy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := callWith(ctx, client, "authorization", "Bearer anything")
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("status = %v (%v), want Unavailable", got, err)
	}
	if spy.calls.Load() != 0 {
		t.Fatal("the handler RAN with no authenticator configured -- the surface failed OPEN")
	}
}
