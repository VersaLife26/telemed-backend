package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/middleware"
)

// Service holds the business rules around the audit repository: default
// pagination, CSV shaping, and chain verification. No http.Request reaches
// this layer.
type Service struct {
	repo *Repository
}

// NewService builds the audit service.
func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Record persists one entry on behalf of an authenticated admin action.
// Called by Middleware, never directly by a handler.
func (s *Service) Record(ctx context.Context, p middleware.Principal, d Draft, ip, userAgent, requestID string) (Entry, error) {
	if d.Action == "" {
		return Entry{}, fmt.Errorf("audit: draft has no action")
	}
	if d.ResourceType == "" {
		d.ResourceType = "unknown"
	}
	return s.repo.Insert(ctx, InsertParams{
		ActorID:      p.UserID,
		ActorRole:    primaryRole(p),
		Action:       d.Action,
		ResourceType: d.ResourceType,
		ResourceID:   d.ResourceID,
		OldValue:     d.OldValue,
		NewValue:     d.NewValue,
		IP:           ip,
		UserAgent:    userAgent,
		RequestID:    requestID,
	})
}

func primaryRole(p middleware.Principal) string {
	// A JWT can carry several realm roles at once. super_admin is recorded
	// over lesser roles when both are present, because it is the role that
	// actually explains why the action was permitted.
	precedence := []middleware.Role{
		middleware.RoleSuperAdmin, middleware.RoleFinance,
		middleware.RoleOps, middleware.RoleSupport, middleware.RoleAdmin,
	}
	for _, r := range precedence {
		if p.HasRole(r) {
			return string(r)
		}
	}
	if len(p.Roles) > 0 {
		return string(p.Roles[0])
	}
	return "unknown"
}

// List returns a page of entries for GET /api/v1/admin/audit.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Entry, int64, error) {
	if f.PerPage <= 0 {
		f.PerPage = 20
	}
	if f.PerPage > 200 {
		f.PerPage = 200
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	return s.repo.List(ctx, f)
}

// Export bounds. Both are deliberate limits on one HTTP response, not on what
// an auditor may ever see: a wider investigation is several calls, each of
// which leaves its own row in the log (see RecordExport). That is the point --
// "download everything, once, invisibly" was the shape of the finding.
const (
	// MaxExportWindow caps from..to. 31 days covers a monthly compliance
	// pull, which is the real use case the endpoint exists for.
	MaxExportWindow = 31 * 24 * time.Hour
	// MaxExportRows caps one response. Sized so a busy month of admin
	// activity fits comfortably; a filter that exceeds it is asking for
	// bulk extraction, not for an investigation.
	MaxExportRows = 100_000
)

// Errors PrepareExport returns for a filter that is not bounded enough to
// export. All three are caller errors, and each message says how to fix it.
var (
	ErrExportUnbounded = errors.New(
		"audit: export requires both from and to (RFC3339); an unbounded export is not permitted")
	ErrExportWindowTooWide = fmt.Errorf(
		"audit: export window must not exceed %d days; narrow from/to and export in several calls",
		int(MaxExportWindow.Hours()/24))
	ErrExportTooLarge = fmt.Errorf(
		"audit: export matches more than %d rows; narrow from/to or add an action/resource filter",
		MaxExportRows)
)

// PrepareExport validates that a filter is bounded enough to export and
// returns how many rows it matches.
//
// It is a separate step from Export on purpose: both the row bound and the
// audit entry describing the export have to be settled BEFORE any bytes go on
// the wire, because once a 200 and a CSV header are written there is no way to
// turn the response into an error. Streaming first and discovering the size
// afterwards would leave the caller with a silently truncated audit export,
// which as evidence is worse than no export at all.
func (s *Service) PrepareExport(ctx context.Context, f ListFilter) (int64, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return 0, ErrExportUnbounded
	}
	if f.To.Sub(f.From) > MaxExportWindow {
		return 0, ErrExportWindowTooWide
	}
	count, err := s.repo.CountMatching(ctx, f)
	if err != nil {
		return 0, err
	}
	if count > MaxExportRows {
		return 0, ErrExportTooLarge
	}
	return count, nil
}

// RecordExport writes the audit entry for a bulk export. An export is itself
// an auditable admin action -- arguably the most interesting one, since it is
// what precedes exfiltration -- and audit.Middleware cannot record it, because
// the middleware deliberately skips GET requests. So the handler records it
// explicitly, before streaming, and refuses to stream if this fails.
func (s *Service) RecordExport(ctx context.Context, p middleware.Principal, f ListFilter, rowCount int64, ip, userAgent, requestID string) (Entry, error) {
	return s.Record(ctx, p, Draft{
		Action:       ActionExported,
		ResourceType: "audit_log",
		NewValue: map[string]any{
			"from":          f.From.UTC().Format(time.RFC3339),
			"to":            f.To.UTC().Format(time.RFC3339),
			"action":        f.Action,
			"resource_type": f.ResourceType,
			"resource_id":   f.ResourceID,
			"row_count":     rowCount,
		},
	}, ip, userAgent, requestID)
}

// Export streams matching entries as CSV rows via emit, oldest first, so an
// operator reading the export sees the story unfold in the order it
// happened. MaxExportRows is applied in SQL as a second line of defence: the
// count PrepareExport took is a moment older than this query, and a burst of
// admin activity in between must not turn into an unbounded response.
func (s *Service) Export(ctx context.Context, f ListFilter, emit func(Entry) error) error {
	return s.repo.StreamExport(ctx, f, MaxExportRows, emit)
}

// Verify walks the hash chain and reports the first broken link, if any.
func (s *Service) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	return s.repo.VerifyChain(ctx, req)
}

// ParseListFilter turns query parameters already extracted by the handler
// into a ListFilter, applying the "from <= to" sanity check services should
// enforce rather than leaving to the repository.
func ParseListFilter(actorID uuid.UUID, action, resourceType, resourceID string, from, to time.Time, page, perPage int) (ListFilter, error) {
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return ListFilter{}, fmt.Errorf("audit: from must not be after to")
	}
	return ListFilter{
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		From:         from,
		To:           to,
		Page:         page,
		PerPage:      perPage,
	}, nil
}
