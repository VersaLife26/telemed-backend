package audit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/database"
	mw "telemed/internal/platform/middleware"
)

// ctxCapturePool records the context the repository actually issues its
// INSERT under. Only QueryRow is ever reached; the embedded nil Pool makes
// any other call a panic rather than a silent pass.
type ctxCapturePool struct {
	database.Pool

	mu     sync.Mutex
	called bool
	ctxErr error
}

func (p *ctxCapturePool) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	p.mu.Lock()
	p.called = true
	p.ctxErr = ctx.Err()
	p.mu.Unlock()
	return stubRow{}
}

func (p *ctxCapturePool) observed() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called, p.ctxErr
}

type stubRow struct{}

func (stubRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if id, ok := dest[0].(*int64); ok {
			*id = 1
		}
	}
	return nil
}

// The audit write must survive the client hanging up.
//
// audit.Middleware runs after the handler has already committed its business
// transaction. If it writes under r.Context(), a client that disconnects the
// instant the handler returns cancels the INSERT -- the state change is
// durable and the record of who made it is not. On an admin console whose
// audit_logs table is hash-chained and REVOKEd precisely so it cannot be
// tampered with, a row that was never written is the cheapest tamper there is.
func TestMiddleware_AuditWriteSurvivesAClientDisconnect(t *testing.T) {
	pool := &ctxCapturePool{}
	svc := audit.NewService(audit.NewRepository(pool))

	// The handler commits, stages its audit draft, and only then does the
	// client vanish -- the exact ordering that loses the row.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	handler := audit.Middleware(svc, zerolog.Nop())(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			audit.Stage(r.Context(), audit.Draft{
				Action:       "admin.updated",
				ResourceType: "admin_user",
				ResourceID:   uuid.NewString(),
			})
			cancel()
			w.WriteHeader(http.StatusOK)
		}))

	ctx := mw.WithPrincipal(cancelledCtx, mw.Principal{UserID: uuid.New(), Roles: []mw.Role{mw.RoleSuperAdmin}})
	req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/admins/"+uuid.NewString(), http.NoBody)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	called, ctxErr := pool.observed()
	require.True(t, called, "the audit INSERT was never attempted")
	require.NoError(t, ctxErr,
		"the audit INSERT ran under a cancelled context (%v); a client disconnect must not erase the record of a committed admin action", ctxErr)
	require.False(t, errors.Is(ctxErr, context.Canceled))
}
