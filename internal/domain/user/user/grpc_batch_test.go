package user

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	userv1 "telemed/internal/pb/user/v1"
)

// TestGetUsersBatch_RejectsAnOversizedBatch closes the leftover named in the
// security review: "GetUsersBatch has no batch-size cap. A compromised service
// token buys a full directory dump in one call."
//
// The proto's repeated user_ids field has no bound and the server sets no
// grpc.MaxRecvMsgSize, so one 4 MiB message carries roughly 110,000 ids -- and
// the method answers with name, phone, email, role and status for every one of
// them. Requiring RoleService establishes WHO may ask. It does nothing about
// HOW MUCH, and how much is what decides whether a leaked mesh token costs one
// screen of PII or the entire user table in a single request that a rate
// limiter counts as one.
func TestGetUsersBatch_RejectsAnOversizedBatch(t *testing.T) {
	// nil service: reaching it would panic, which is deliberate. The cap has to
	// bite before any work happens, not after the query has already run.
	g := NewGRPCServer(nil, zerolog.Nop())

	ids := make([]string, MaxUsersBatch+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}

	_, err := g.GetUsersBatch(context.Background(), &userv1.GetUsersBatchRequest{UserIds: ids})
	if err == nil {
		t.Fatalf("a batch of %d ids was accepted; the cap is %d", len(ids), MaxUsersBatch)
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument so the caller knows to page", got)
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Fatalf("the error should state the limit, got: %v", err)
	}
}

// TestGetUsersBatch_ServiceLayerEnforcesTheCapToo pins the structural half.
//
// The transport check produces the good error and the log line; this one is
// what still holds when a second caller appears. A limit that lives only in the
// handler is a limit the next refactor deletes without noticing -- which is
// exactly how the uncapped version existed in the first place.
func TestGetUsersBatch_ServiceLayerEnforcesTheCapToo(t *testing.T) {
	s := &Service{}
	ids := make([]uuid.UUID, MaxUsersBatch+1)
	for i := range ids {
		ids[i] = uuid.New()
	}
	// nil repo: if the guard is removed this reaches s.repo and panics rather
	// than quietly returning, so the test cannot pass for the wrong reason.
	_, err := s.GetUsersBatch(context.Background(), ids)
	if err == nil {
		t.Fatal("the service layer accepted an oversized batch")
	}
	if !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("err = %v, want ErrBatchTooLarge", err)
	}
}

// TestGetUsersBatch_CapMatchesThePlatformPageSize keeps the number honest.
func TestGetUsersBatch_CapMatchesThePlatformPageSize(t *testing.T) {
	if MaxUsersBatch != 100 {
		t.Fatalf("MaxUsersBatch = %d, want 100 to match httpx.Pagination's hard cap", MaxUsersBatch)
	}
}
