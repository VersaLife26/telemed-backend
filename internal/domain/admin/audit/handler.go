package audit

import (
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/csvx"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
	adminmw "telemed/internal/platform/middleware"
)

// Handler wires the audit service to HTTP. It runs no SQL.
//
// RequireAuth, IPAllowlist and the subtree's RequireRole are applied by
// main.go before requests reach here. The one authorization decision this
// package makes for itself is the narrower role gate on GET /export, composed
// in Routes below -- it lives here, next to the route, because the previous
// arrangement was a doc comment asserting the router applied a check that the
// router did not apply (security review F6). A control described in one file
// and implemented in none is worse than no control, because it reads as done.
type Handler struct {
	svc *Service
	// exportRoles are the roles permitted to bulk-export. main.go passes
	// rbac.Roles(rbac.GroupAuditExport) so the tested matrix and the enforced
	// route stay the same list. Empty would mean "no role gate", so
	// NewHandler refuses to build a handler with an empty list rather than
	// failing open.
	exportRoles []adminmw.Role
}

// NewHandler builds the audit HTTP handler. exportRoles must be non-empty; a
// caller that passes nothing gets the narrowest possible gate (super_admin)
// rather than an open route, for the same reason rbac.Roles falls back that
// way.
func NewHandler(svc *Service, exportRoles ...adminmw.Role) *Handler {
	if len(exportRoles) == 0 {
		exportRoles = []adminmw.Role{adminmw.RoleSuperAdmin}
	}
	return &Handler{svc: svc, exportRoles: exportRoles}
}

// Routes mounts GET /, GET /export and POST /verify under whatever prefix
// the caller mounts this at (main.go mounts it at /api/v1/admin/audit).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.With(adminmw.RequireRole(h.exportRoles...)).Get("/export", h.export)
	r.Post("/verify", h.verify)
	return r
}

// EntryDTO is the wire shape. Kept separate from the domain Entry so a
// column rename in the repository never silently changes the public API.
type EntryDTO struct {
	ID           int64     `json:"id"`
	ActorID      string    `json:"actor_id,omitempty"`
	ActorRole    string    `json:"actor_role"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id,omitempty"`
	OldValue     any       `json:"old_value,omitempty"`
	NewValue     any       `json:"new_value,omitempty"`
	IP           string    `json:"ip,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	PrevHash     string    `json:"prev_hash"`
	RowHash      string    `json:"row_hash"`
}

// ToDTO maps a persisted row onto the public JSON shape.
func ToDTO(e Entry) EntryDTO {
	d := EntryDTO{
		ID: e.ID, ActorRole: e.ActorRole, Action: e.Action,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		IP: e.IP, UserAgent: e.UserAgent, RequestID: e.RequestID,
		CreatedAt: e.CreatedAt, PrevHash: e.PrevHash, RowHash: e.RowHash,
	}
	if e.ActorID != uuid.Nil {
		d.ActorID = e.ActorID.String()
	}
	if len(e.OldValue) > 0 {
		d.OldValue = jsonRaw(e.OldValue)
	}
	if len(e.NewValue) > 0 {
		d.NewValue = jsonRaw(e.NewValue)
	}
	return d
}

// jsonRaw lets json.Marshal re-embed already-marshalled JSON verbatim.
type jsonRaw []byte

func (j jsonRaw) MarshalJSON() ([]byte, error) { return j, nil }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var actorID uuid.UUID
	if raw := q.Get("actor_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "actor_id must be a valid UUID"))
			return
		}
		actorID = id
	}

	from, err := parseOptionalTime(q.Get("from"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "from must be RFC3339"))
		return
	}
	to, err := parseOptionalTime(q.Get("to"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to must be RFC3339"))
		return
	}

	page, perPage, _ := httpx.Pagination(r)
	filter, err := ParseListFilter(actorID, q.Get("action"), q.Get("resource_type"), q.Get("resource_id"), from, to, page, perPage)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	}

	entries, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	dtos := make([]EntryDTO, len(entries))
	for i := range entries {
		dtos[i] = ToDTO(entries[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// export streams the audit trail as CSV.
//
// Three things had to change here (security review F6). The route was open to
// all five admin roles (fixed in Routes, above). The query had no bound at all,
// so one request returned the entire table. And because audit.Middleware skips
// GETs, the export left no trace in the log it was reading -- an export that
// records nothing is the one an attacker uses.
//
// The order below is deliberate: validate, bound, then WRITE THE AUDIT ROW,
// and only then start streaming. Auditing after the fact would lose the record
// exactly when it matters most, on a client that disconnects mid-download. If
// the audit write fails, the export does not happen: this endpoint fails
// closed, because a silent export is the thing being prevented.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	from, err := parseOptionalTime(q.Get("from"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "from must be RFC3339"))
		return
	}
	to, err := parseOptionalTime(q.Get("to"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to must be RFC3339"))
		return
	}
	filter, err := ParseListFilter(uuid.Nil, q.Get("action"), q.Get("resource_type"), q.Get("resource_id"), from, to, 0, 0)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	}

	rowCount, err := h.svc.PrepareExport(r.Context(), filter)
	if err != nil {
		httpx.Error(w, r, exportError(err))
		return
	}

	principal, _ := adminmw.PrincipalFrom(r.Context())
	if _, err := h.svc.RecordExport(r.Context(), principal, filter, rowCount,
		adminmw.ClientIP(r), r.UserAgent(), chimw.GetReqID(r.Context())); err != nil {
		httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-export.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "actor_id", "actor_role", "action", "resource_type", "resource_id",
		"old_value", "new_value", "ip", "user_agent", "request_id", "created_at",
		"prev_hash", "row_hash",
	})

	emitErr := h.svc.Export(r.Context(), filter, func(e Entry) error {
		return cw.Write(exportRow(e))
	})
	cw.Flush()

	if emitErr := errors.Join(emitErr, cw.Error()); emitErr != nil {
		// The 200 and the CSV header are already on the wire, so this cannot
		// become an error response. It must not become a SUCCESSFUL one
		// either: a CSV that stops early is indistinguishable from a complete
		// export of a shorter period, and this file is evidence. Aborting the
		// connection makes the client see a failed transfer.
		// http.ErrAbortHandler is the documented way to do that, and
		// platform/server's Recoverer already treats it as a deliberate abort
		// rather than a crash. The audit row naming the requested scope was
		// written before streaming began, so the attempt is on record either
		// way.
		log := logger.FromContext(r.Context())
		log.Error().Err(emitErr).
			Int64("row_count", rowCount).
			Msg("audit export failed mid-stream; aborting the response so a truncated CSV cannot look complete")
		panic(http.ErrAbortHandler)
	}
}

// exportRow renders one entry as a CSV record.
//
// csvx.Row, not a bare slice. This file is handed to a regulator, and
// old_value/new_value/user_agent carry text somebody else authored -- a
// dispute description, a doctor rejection reason, a config value, a
// User-Agent. A cell beginning "=", "+", "-", "@", TAB or CR executes when the
// file is opened in Excel or LibreOffice, so an attacker who can get a string
// into any audited field gets code execution on the auditor's machine, out of
// a file the platform vouched for.
//
// The chain columns are hex and can never begin with one of those characters,
// so they pass through byte for byte. On whether this weakens chain
// verification: it cannot, because the CSV was never verifiable from its own
// bytes. audit_canonical_json formats created_at to fixed microseconds while
// this emits RFC3339Nano, which drops trailing zeros, so recomputing SHA-256
// from these bytes already failed on any row whose timestamp ends in a zero.
// Chain verification is POST /admin/audit/verify, which recomputes in SQL
// against the table.
func exportRow(e Entry) []string {
	actorID := ""
	if e.ActorID != uuid.Nil {
		actorID = e.ActorID.String()
	}
	return csvx.Row([]string{
		strconv.FormatInt(e.ID, 10), actorID, e.ActorRole, e.Action, e.ResourceType, e.ResourceID,
		string(e.OldValue), string(e.NewValue), e.IP, e.UserAgent, e.RequestID,
		e.CreatedAt.UTC().Format(time.RFC3339Nano), e.PrevHash, e.RowHash,
	})
}

type verifyRequestDTO struct {
	FromID int64 `json:"from_id"`
	Limit  int   `json:"limit"`
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	var body verifyRequestDTO
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &body); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	result, err := h.svc.Verify(r.Context(), VerifyRequest(body))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func parseOptionalTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

// exportError maps the service's export-bound errors onto the wire. They are
// 400s, not 500s: the caller asked for more than this endpoint will hand over
// in one response, and the message says exactly how to ask for less.
func exportError(err error) error {
	switch {
	case errors.Is(err, ErrExportUnbounded),
		errors.Is(err, ErrExportWindowTooWide),
		errors.Is(err, ErrExportTooLarge):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
	default:
		return err
	}
}
