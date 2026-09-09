package user

import (
	"context"

	"google.golang.org/grpc"

	userv1 "telemed/internal/pb/user/v1"
)

// InProcessClient is a userv1.UserServiceClient that calls this process's own
// UserService implementation instead of dialling it.
//
// # WHY THE MESH TOKEN IS NOT CHECKED HERE, AND WHY THAT IS NOT A HOLE
//
// The gRPC listener authenticates every method and exempts none
// (GRPCServerOptions, security review F5). That interceptor exists for one
// reason, stated in middleware/grpcauth.go: these ports are never exposed
// publicly, but "not routed from the internet" is a network accident, not an
// access control. A misapplied NetworkPolicy, a debug port-forward, or one
// compromised pod is all it takes for something to open a TCP connection to
// :9091 and dump the platform's entire patient directory.
//
// This type is not reachable that way. It is a Go method call from another
// package in the same binary, and there is no credential a caller could
// present or withhold that would mean anything: the caller is already inside
// the trust boundary the token was proving membership of. Demanding a token
// here would mean the process minting one for itself and immediately verifying
// it, which authenticates nothing and reads as security only until someone
// looks at it.
//
// What DOES have to stay true, and is asserted by grpc_auth_test.go:
//
//   - the listener keeps its interceptor whenever it is started, so a process
//     that exposes :9091 is no more reachable than it was before;
//   - this adapter is only handed out inside the process that owns the user
//     domain -- cmd/telemed registers it only when the user domain is loaded.
//     A process WITHOUT the user domain gets the real gRPC client and
//     therefore the real token requirement, unchanged.
//
// The authorisation that is genuinely per-caller -- which fields a role may
// see, whether a suspended account resolves -- was never in the interceptor.
// It lives in Service, which this calls, so it applies identically either way.
type InProcessClient struct {
	srv userv1.UserServiceServer
}

// NewInProcessClient wraps a server implementation as a client.
func NewInProcessClient(srv userv1.UserServiceServer) *InProcessClient {
	return &InProcessClient{srv: srv}
}

var _ userv1.UserServiceClient = (*InProcessClient)(nil)

// GetUser resolves one user.
//
// CallOptions are accepted and ignored. Every one of them -- deadlines are
// already on the context, compression, per-call credentials, retries -- is a
// property of a wire hop that is not happening. Silently ignoring them is
// correct here and would not be for a client that might dial.
func (c *InProcessClient) GetUser(ctx context.Context, in *userv1.GetUserRequest, _ ...grpc.CallOption) (*userv1.GetUserResponse, error) {
	return c.srv.GetUser(ctx, in)
}

// GetUsersBatch resolves many users in one call.
func (c *InProcessClient) GetUsersBatch(ctx context.Context, in *userv1.GetUsersBatchRequest, _ ...grpc.CallOption) (*userv1.GetUsersBatchResponse, error) {
	return c.srv.GetUsersBatch(ctx, in)
}
