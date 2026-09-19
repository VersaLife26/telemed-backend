package doctor

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// NameDirectory resolves doctor profile ids to display names for screens
// that must show who a visit is with.
type NameDirectory struct {
	repo *Repository
	log  zerolog.Logger
}

// NewNameDirectory builds a directory over the doctors table.
func NewNameDirectory(repo *Repository, log zerolog.Logger) *NameDirectory {
	return &NameDirectory{repo: repo, log: log.With().Str("component", "doctor-names").Logger()}
}

// Names returns the display name of each id it could resolve. A failed
// lookup is logged and leaves those ids out: a missing label must never fail
// the request it decorates.
func (d *NameDirectory) Names(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if d == nil || d.repo == nil || len(ids) == 0 {
		return out
	}
	doctors, err := d.repo.ListByIDs(ctx, ids)
	if err != nil {
		d.log.Warn().Err(err).Int("count", len(ids)).Msg("doctor name lookup failed")
		return out
	}
	for i := range doctors {
		if doctors[i].DisplayName != "" {
			out[doctors[i].ID] = doctors[i].DisplayName
		}
	}
	return out
}
