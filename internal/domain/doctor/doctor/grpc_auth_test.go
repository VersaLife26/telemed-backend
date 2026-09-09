package doctor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	doctorv1 "telemed/internal/pb/doctor/v1"
	"telemed/internal/platform/middleware"
)

// These tests drive a REAL gRPC server over a REAL TCP connection, assembled
// from GRPCServerOptions -- the same function cmd/server/main.go hands to
// grpc.NewServer. Asserting against an interceptor chain the test built itself
// would prove something about the test and nothing about :9092.
//
// The finding: this listener was a bare grpc.NewServer(). No UnaryInterceptor,
// no ChainUnaryInterceptor, no grpc.Creds. Anything that could open a TCP
// connection to the port had full authority over DoctorService, and the only
// control was a NetworkPolicy -- which is a property of the network, not an
// access decision.
//
// Every case asserts the status code AND whether the handler was reached,
// because "the caller got an error" and "the database was never read" are
// different claims and only the second one is a security property.

type spyDoctorServer struct {
	doctorv1.UnimplementedDoctorServiceServer
	calls atomic.Int64
	seen  atomic.Value // middleware.Principal
}

func (s *spyDoctorServer) GetDoctor(ctx context.Context, _ *doctorv1.GetDoctorRequest) (*doctorv1.GetDoctorResponse, error) {
	s.calls.Add(1)
	if p, ok := middleware.PrincipalFrom(ctx); ok {
		s.seen.Store(p)
	}
	return &doctorv1.GetDoctorResponse{}, nil
}

const (
	testMeshIssuer   = "test-mesh-issuer"
	testMeshAudience = "telemed-api"
)

// meshSigner mints genuine RS256 tokens and publishes the matching JWKS, so
// middleware.Authenticator does exactly what it does in production: fetch a key
// set over HTTP and bind it to one issuer.
type meshSigner struct {
	key  *rsa.PrivateKey
	kid  string
	iss  string
	aud  string
	jwks string
}

func newMeshSigner(t *testing.T) *meshSigner {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s := &meshSigner{key: key, kid: "mesh-test-1", iss: testMeshIssuer, aud: testMeshAudience}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": s.kid,
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	s.jwks = srv.URL

	return s
}

// token mints a correctly signed, unexpired token carrying exactly the given
// roles. Nothing below is a forgery: the point is that a genuine credential
// still does not open this door unless it carries the service role.
func (s *meshSigner) token(t *testing.T, userID uuid.UUID, roles ...string) string {
	t.Helper()

	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"sub":             userID.String(),
		"telemed_user_id": userID.String(),
		"iss":             s.iss,
		"aud":             s.aud,
		"iat":             now.Unix(),
		"exp":             now.Add(10 * time.Minute).Unix(),
		"realm_access":    map[string]any{"roles": roles},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.kid

	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func (s *meshSigner) authenticator(t *testing.T) *middleware.Authenticator {
	t.Helper()
	auth, err := middleware.NewAuthenticatorFrom(context.Background(), middleware.AuthConfig{
		IssuerKeys: map[string]string{s.iss: s.jwks},
		Audience:   s.aud,
	})
	if err != nil {
		t.Fatalf("authenticator: %v", err)
	}
	return auth
}

func startDoctorGRPC(t *testing.T, auth *middleware.Authenticator, spy *spyDoctorServer) doctorv1.DoctorServiceClient {
	t.Helper()

	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(GRPCServerOptions(auth)...)
	doctorv1.RegisterDoctorServiceServer(srv, spy)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return doctorv1.NewDoctorServiceClient(conn)
}

func callDoctor(ctx context.Context, c doctorv1.DoctorServiceClient, md ...string) error {
	if len(md) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, md...)
	}
	_, err := c.GetDoctor(ctx, &doctorv1.GetDoctorRequest{DoctorId: uuid.NewString()})
	return err
}

func TestGRPC_AnonymousCallerIsRefusedAndTheHandlerNeverRuns(t *testing.T) {
	t.Parallel()

	signer := newMeshSigner(t)
	spy := &spyDoctorServer{}
	client := startDoctorGRPC(t, signer.authenticator(t), spy)

	cases := []struct {
		name string
		md   []string
	}{
		{name: "no metadata at all -- the exploit as reported"},
		{name: "metadata but no authorization", md: []string{"user-agent", "grpcurl/1.9"}},
		{name: "token with no Bearer scheme", md: []string{"authorization", signer.token(t, uuid.New(), "service")}},
		{name: "wrong scheme", md: []string{"authorization", "Basic " + signer.token(t, uuid.New(), "service")}},
		{name: "Bearer with an empty token", md: []string{"authorization", "Bearer "}},
		{name: "an alg:none token", md: []string{"authorization", "Bearer eyJhbGciOiJub25lIn0.eyJzdWIiOiJhIn0."}},
		{name: "garbage", md: []string{"authorization", "Bearer not-a-jwt"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := spy.calls.Load()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := callDoctor(ctx, client, tc.md...)
			if got := status.Code(err); got != codes.Unauthenticated {
				t.Fatalf("status = %v (%v), want Unauthenticated", got, err)
			}
			if spy.calls.Load() != before {
				t.Fatal("the handler RAN for an unauthenticated caller")
			}
		})
	}
}

func TestGRPC_APatientOrDoctorTokenIsNotAMeshCredential(t *testing.T) {
	t.Parallel()

	signer := newMeshSigner(t)
	spy := &spyDoctorServer{}
	client := startDoctorGRPC(t, signer.authenticator(t), spy)

	// This service accepts patient and doctor tokens on its HTTP surface
	// (ADR-010), so without the role gate every one of them would also be a
	// key to the internal mesh. The admin roles are here for the same reason:
	// the mesh is machine-to-machine, and a human credential is not one.
	for _, role := range []string{"patient", "doctor", "admin", "super_admin", "ops", "finance", "support"} {
		t.Run(role, func(t *testing.T) {
			before := spy.calls.Load()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := callDoctor(ctx, client, "authorization", "Bearer "+signer.token(t, uuid.New(), role))
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Fatalf("a genuine %q token got %v (%v), want PermissionDenied", role, got, err)
			}
			if spy.calls.Load() != before {
				t.Fatalf("the handler RAN for a %q token", role)
			}
		})
	}
}

func TestGRPC_ServiceTokenWorksAndTheHandlerSeesTheVerifiedCaller(t *testing.T) {
	t.Parallel()

	signer := newMeshSigner(t)
	spy := &spyDoctorServer{}
	client := startDoctorGRPC(t, signer.authenticator(t), spy)

	callerID := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := callDoctor(ctx, client, "authorization", "Bearer "+signer.token(t, callerID, "service")); err != nil {
		t.Fatalf("a legitimate service token was refused: %v", err)
	}
	if spy.calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 -- the legitimate path must still work", spy.calls.Load())
	}

	p, ok := spy.seen.Load().(middleware.Principal)
	if !ok {
		t.Fatal("no principal on the handler's context: a handler has no way to read the caller's identity")
	}
	if p.UserID != callerID {
		t.Fatalf("principal.UserID = %s, want %s", p.UserID, callerID)
	}
	if !p.HasRole(middleware.RoleService) {
		t.Fatalf("principal roles = %v, want the service role", p.Roles)
	}
	if p.Issuer != testMeshIssuer {
		t.Fatalf("principal.Issuer = %q, want %q -- the issuer must be a verified fact", p.Issuer, testMeshIssuer)
	}
}

func TestGRPC_ATokenFromAnUntrustedIssuerIsRefused(t *testing.T) {
	t.Parallel()

	// A second signer, with its own key and its own issuer name, that the
	// server has never been told about. This is the shape of a compromised
	// neighbouring service trying to talk its way in.
	trusted := newMeshSigner(t)
	rogue := newMeshSigner(t)
	rogue.iss = "rogue-issuer"

	spy := &spyDoctorServer{}
	client := startDoctorGRPC(t, trusted.authenticator(t), spy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := callDoctor(ctx, client, "authorization", "Bearer "+rogue.token(t, uuid.New(), "service"))
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("status = %v (%v), want Unauthenticated", got, err)
	}
	if spy.calls.Load() != 0 {
		t.Fatal("the handler RAN for a token signed by a key this server does not trust")
	}
}

func TestGRPC_MisconfiguredAuthenticationFailsClosed(t *testing.T) {
	t.Parallel()

	// A deployment whose JWKS could not be loaded at boot. Serving the mesh
	// unauthenticated because authentication failed to configure is the one
	// outcome that is never acceptable.
	spy := &spyDoctorServer{}
	client := startDoctorGRPC(t, nil, spy)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := callDoctor(ctx, client, "authorization", "Bearer anything")
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("status = %v (%v), want Unavailable", got, err)
	}
	if spy.calls.Load() != 0 {
		t.Fatal("the handler RAN with no authenticator configured -- the surface failed OPEN")
	}
}
