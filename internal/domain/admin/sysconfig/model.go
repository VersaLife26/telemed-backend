// Package sysconfig implements versioned platform configuration: commission
// rules, cancellation policy, fee caps, feature flags. A PUT never mutates a
// previous version -- it inserts a new one, and migrations/000003_admin_core
// backs that with a trigger that rejects UPDATE/DELETE on system_configs
// outright, not merely a repository that happens to avoid writing one.
package sysconfig

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Config is one version of one key.
type Config struct {
	ID            int64
	Key           string
	Value         json.RawMessage
	Version       int
	UpdatedBy     uuid.UUID
	EffectiveFrom time.Time
	CreatedAt     time.Time
}
