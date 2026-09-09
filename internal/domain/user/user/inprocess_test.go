package user_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"telemed/internal/domain/user/user"
	userv1 "telemed/internal/pb/user/v1"
)

// recordingServer is the smallest UserServiceServer that can prove delegation.
type recordingServer struct {
	userv1.UnimplementedUserServiceServer
	getCalls   int
	batchCalls int
	lastID     string
	lastIDs    []string
	err        error
}

func (s *recordingServer) GetUser(_ context.Context, in *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	s.getCalls++
	s.lastID = in.GetUserId()
	if s.err != nil {
		return nil, s.err
	}
	return &userv1.GetUserResponse{User: &userv1.User{Id: in.GetUserId()}}, nil
}

func (s *recordingServer) GetUsersBatch(_ context.Context, in *userv1.GetUsersBatchRequest) (*userv1.GetUsersBatchResponse, error) {
	s.batchCalls++
	s.lastIDs = in.GetUserIds()
	if s.err != nil {
		return nil, s.err
	}
	return &userv1.GetUsersBatchResponse{}, nil
}

// The adapter must satisfy the generated client interface, or the domains that
// resolve users through it would need a second code path.
func TestInProcessClient_SatisfiesTheGeneratedClientInterface(t *testing.T) {
	t.Parallel()
	var _ userv1.UserServiceClient = user.NewInProcessClient(&recordingServer{})
}

func TestInProcessClient_DelegatesToTheServerImplementation(t *testing.T) {
	t.Parallel()
	srv := &recordingServer{}
	c := user.NewInProcessClient(srv)

	resp, err := c.GetUser(context.Background(), &userv1.GetUserRequest{UserId: "u-1"})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if srv.getCalls != 1 || srv.lastID != "u-1" {
		t.Fatalf("GetUser did not reach the implementation: calls=%d id=%q", srv.getCalls, srv.lastID)
	}
	if got := resp.GetUser().GetId(); got != "u-1" {
		t.Fatalf("response not passed back: got %q", got)
	}

	if _, err := c.GetUsersBatch(context.Background(),
		&userv1.GetUsersBatchRequest{UserIds: []string{"a", "b"}}); err != nil {
		t.Fatalf("GetUsersBatch: %v", err)
	}
	if srv.batchCalls != 1 || len(srv.lastIDs) != 2 {
		t.Fatalf("GetUsersBatch did not reach the implementation: calls=%d ids=%v", srv.batchCalls, srv.lastIDs)
	}
}

// An error from the implementation must arrive at the caller unchanged.
//
// The directory clients in the admin and notification domains branch on the
// gRPC status of a failure -- NOT_FOUND is "this user does not exist" and stops,
// anything else is "user-service is unreachable" and retries. An adapter that
// wrapped or flattened the error would turn a stale reference into a retry loop.
func TestInProcessClient_PassesErrorsBackUnchanged(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("not found")
	c := user.NewInProcessClient(&recordingServer{err: sentinel})

	if _, err := c.GetUser(context.Background(), &userv1.GetUserRequest{UserId: "u-1"}); !errors.Is(err, sentinel) {
		t.Fatalf("GetUser error was not passed through: %v", err)
	}
	if _, err := c.GetUsersBatch(context.Background(), &userv1.GetUsersBatchRequest{}); !errors.Is(err, sentinel) {
		t.Fatalf("GetUsersBatch error was not passed through: %v", err)
	}
}

// CallOptions are accepted and ignored: every one of them describes a wire hop
// that is not happening. Asserted so the behaviour is a decision on the record
// rather than something a future reader has to infer from an unused parameter.
func TestInProcessClient_IgnoresCallOptions(t *testing.T) {
	t.Parallel()
	srv := &recordingServer{}
	c := user.NewInProcessClient(srv)

	if _, err := c.GetUser(context.Background(), &userv1.GetUserRequest{UserId: "u-1"},
		grpc.WaitForReady(true), grpc.MaxCallRecvMsgSize(1)); err != nil {
		t.Fatalf("GetUser with call options: %v", err)
	}
	if srv.getCalls != 1 {
		t.Fatalf("call options changed delegation: calls=%d", srv.getCalls)
	}
}
