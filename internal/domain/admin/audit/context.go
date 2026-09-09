package audit

import (
	"context"
	"sync"

	"telemed/internal/platform/logger"
)

type boxKey struct{}

// box is a mutable container installed into the request context by
// Middleware before the handler chain runs. Service-layer code stages
// drafts into it via Stage; the middleware reads them back out after the
// handler returns and persists them. context.Context itself is immutable,
// so this indirection -- a pointer the context merely carries -- is what
// lets a deeply nested service call influence what the middleware writes
// without threading a return value through every layer by hand.
type box struct {
	mu     sync.Mutex
	drafts []Draft
}

func withBox(ctx context.Context) (context.Context, *box) {
	b := &box{}
	return context.WithValue(ctx, boxKey{}, b), b
}

// Stage records the semantic change (action, resource, old/new values) a
// service just made. Middleware fills in who made it, from where, and when,
// then persists it. Call it once the business transaction has committed --
// staging a change that then fails to commit would audit something that
// never happened.
//
// Stage is a no-op (loudly, via a warning log) outside a request that passed
// through Middleware, which in practice means "outside an admin HTTP
// request" -- background workers and event consumers do not mutate
// admin-console-owned state on a human's behalf and so have nothing to
// attribute an audit entry to.
func Stage(ctx context.Context, d Draft) {
	b, ok := ctx.Value(boxKey{}).(*box)
	if !ok {
		log := logger.FromContext(ctx)
		log.Warn().Str("action", d.Action).
			Msg("audit.Stage called outside audit.Middleware; entry was NOT written")
		return
	}
	b.mu.Lock()
	b.drafts = append(b.drafts, d)
	b.mu.Unlock()
}

func drain(ctx context.Context) ([]Draft, bool) {
	b, ok := ctx.Value(boxKey{}).(*box)
	if !ok {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drafts, true
}
