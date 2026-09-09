package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	doctorv1 "telemed/internal/pb/doctor/v1"
)

// TestGetDoctorsBatch_RejectsAnOversizedBatch caps a method that had no bound
// on how much work one request could ask for.
//
// The proto's repeated doctor_ids field carries no limit and the server sets no
// grpc.MaxRecvMsgSize, so the ceiling was gRPC's 4 MiB default -- roughly
// 110,000 UUIDs in a single message. The loop then ran one
// `SELECT ... WHERE id = $1` PER ID, sequentially, holding a pool connection
// for the whole traversal. One RPC was therefore up to 110,000 round trips: a
// database denial of service that needs no volume of requests at all, just one
// large one, from any caller holding a mesh service token.
//
// It is also a bulk read of doctor identity -- SLMC registration number
// included -- and requiring RoleService says WHO may ask, not HOW MUCH they
// may ask for. Only the second bounds what a leaked service token is worth.
func TestGetDoctorsBatch_RejectsAnOversizedBatch(t *testing.T) {
	g := NewGRPCServer(nil, zerolog.Nop())

	ids := make([]string, MaxDoctorsBatch+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}

	_, err := g.GetDoctorsBatch(context.Background(), &doctorv1.GetDoctorsBatchRequest{DoctorIds: ids})
	if err == nil {
		t.Fatalf("a batch of %d ids was accepted; the cap is %d", len(ids), MaxDoctorsBatch)
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument (the caller must be able to fix this by paging)", got)
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Fatalf("the error should tell the caller the limit, got: %v", err)
	}
	// nil repo: reaching the repository at all would panic, which is the point
	// -- the cap must be checked before any work is done, not after.
}

// TestGetDoctorsBatch_CapMatchesThePlatformPageSize keeps the number honest.
// httpx.Pagination clamps per_page to 100, and every legitimate caller of this
// method is resolving the doctors on one page of appointments.
func TestGetDoctorsBatch_CapMatchesThePlatformPageSize(t *testing.T) {
	if MaxDoctorsBatch != 100 {
		t.Fatalf("MaxDoctorsBatch = %d, want 100 to match httpx.Pagination's hard cap", MaxDoctorsBatch)
	}
}
