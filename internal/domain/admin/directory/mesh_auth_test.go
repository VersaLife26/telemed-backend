package directory_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"telemed/internal/domain/admin/directory"
	userv1 "telemed/internal/pb/user/v1"
)

// requireBearer is the shape of user-service's UnaryServiceAuth as it applies
// to this client: every method needs an Authorization bearer, there are no
// exempt methods, and a caller that presents nothing is refused before the
// handler runs. Reproduced here rather than imported because admin-service is
// a gRPC CLIENT and its platform copy carries no grpcauth.go -- which is
// itself part of why nothing in this repo noticed the requirement appear.
func requireBearer(
	ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("authorization")) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no service token")
	}
	return handler(ctx, req)
}

// stubUserServer answers GetUser. It never runs in these tests unless the
// interceptor lets the call through, which is the point.
type stubUserServer struct {
	userv1.UnimplementedUserServiceServer
	reached bool
}

func (s *stubUserServer) GetUser(_ context.Context, req *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	s.reached = true
	return &userv1.GetUserResponse{User: &userv1.User{
		Id: req.GetUserId(), Name: "Nadeesha Perera",
		Email: "n@example.lk", Phone: "+94771234567",
		Role: "patient", Status: "active",
	}}, nil
}

// staticCreds presents a fixed bearer, standing in for the Keycloak
// client_credentials token servicetoken.Source fetches in production.
type staticCreds struct{ token string }

func (c staticCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

// false: the test server is plaintext. Production sets RequireTLS from the
// mesh TLS setting, so gRPC refuses to attach the credential to an insecure
// connection rather than leaking it.
func (c staticCreds) RequireTransportSecurity() bool { return false }

// TestDirectoryClientPresentsAServiceToken is the fix for the break this file
// originally documented.
//
// The F5 fix put middleware.UnaryServiceAuth in front of user-service's gRPC
// surface with RequiredRole=service and NO exempt methods -- correct, because
// that surface is a full PII directory. But this service still dials it with
// grpc.NewClient(addr, WithTransportCredentials(...)) and nothing else: no
// per-RPC credentials, no bearer metadata, no service token.
//
// So every admin-service user lookup now returns Unauthenticated, and the
// console's user projection has stopped learning names, emails and phones.
// Neither service fails to build or start, which is exactly why it went
// unnoticed: the break is entirely at the call.
//
// The client now carries credentials.PerRPCCredentials, supplied at
// construction rather than as an option -- it was the ABSENCE of a credential
// that broke this, so making the client impossible to build without deciding
// about one is the actual fix. This test proves the call reaches the handler.
func TestDirectoryClientPresentsAServiceToken(t *testing.T) {
	stub := &stubUserServer{}

	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(requireBearer))
	userv1.RegisterUserServiceServer(srv, stub)

	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := directory.New(lis.Addr().String(), 2*time.Second, directory.TLSConfig{},
		staticCreds{token: "service-token"})
	if err != nil {
		t.Fatalf("directory.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	got, err := client.User(t.Context(), uuid.New())
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			t.Fatalf("the mesh interceptor refused the call: the client is not "+
				"presenting its service token (%v)", err)
		}
		t.Fatalf("User: %v", err)
	}
	if !stub.reached {
		t.Fatal("the handler never ran, so the call did not get past the interceptor")
	}
	if got.FullName != "Nadeesha Perera" {
		t.Errorf("FullName = %q, want the stub's value: the response was not decoded", got.FullName)
	}
}

// TestDirectoryClientWithoutCredentialsIsRefused keeps the original finding
// under test. user-service authenticates every gRPC method with no exemptions
// -- correct for a full PII directory -- so a client built without a
// credential must be refused rather than quietly answered.
func TestDirectoryClientWithoutCredentialsIsRefused(t *testing.T) {
	stub := &stubUserServer{}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(requireBearer))
	userv1.RegisterUserServiceServer(srv, stub)

	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := directory.New(lis.Addr().String(), 2*time.Second, directory.TLSConfig{}, nil)
	if err != nil {
		t.Fatalf("directory.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.User(t.Context(), uuid.New())
	if err == nil {
		t.Fatal("an uncredentialed call succeeded; the mesh is not authenticating")
	}
	if stub.reached {
		t.Fatal("the handler ran for an uncredentialed caller")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Fatalf("got %v, want Unauthenticated", code)
	}
	if errors.Is(err, directory.ErrNotFound) {
		t.Fatal("a rejected call was mapped to ErrNotFound, which would make an " +
			"auth failure look like a user who does not exist")
	}
}
