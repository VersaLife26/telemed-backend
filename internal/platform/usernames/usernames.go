// Package usernames resolves user ids to display names through the user
// domain's directory, for screens that must show "who" beside an id.
//
// It only ever uses the IN-PROCESS directory the user domain publishes into
// the modular registry. A process without the user domain gets no resolver
// and names come back empty; the caller shows a placeholder rather than a
// domain growing its own mesh-credentialed gRPC client just to label a row.
package usernames

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/modular"
)

// batchSize matches user.MaxUsersBatch, which rejects anything larger.
const batchSize = 100

const lookupTimeout = 2 * time.Second

// Resolver looks names up in batches.
type Resolver struct {
	client userv1.UserServiceClient
	log    zerolog.Logger
}

// FromRegistry returns a resolver backed by the in-process user directory,
// or nil when the user domain is not loaded in this process.
func FromRegistry(reg *modular.Registry, log zerolog.Logger) *Resolver {
	v, ok := reg.Lookup(modular.KeyUserDirectory)
	if !ok {
		return nil
	}
	client, ok := v.(userv1.UserServiceClient)
	if !ok {
		return nil
	}
	return &Resolver{client: client, log: log.With().Str("component", "usernames").Logger()}
}

// Names returns the display name of each id it could resolve. A failed
// lookup is logged and leaves those ids out: a missing label must never fail
// the request it decorates.
func (r *Resolver) Names(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if r == nil || len(ids) == 0 {
		return out
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id.String())
	}
	for start := 0; start < len(unique); start += batchSize {
		end := min(start+batchSize, len(unique))
		cctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		resp, err := r.client.GetUsersBatch(cctx, &userv1.GetUsersBatchRequest{UserIds: unique[start:end]})
		cancel()
		if err != nil {
			r.log.Warn().Err(err).Int("count", end-start).Msg("user name lookup failed")
			continue
		}
		for _, u := range resp.GetUsers() {
			if id, err := uuid.Parse(u.GetId()); err == nil && u.GetName() != "" {
				out[id] = u.GetName()
			}
		}
	}
	return out
}
