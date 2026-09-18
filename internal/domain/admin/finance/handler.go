package finance

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/csvx"
	"telemed/internal/domain/admin/sysconfig"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
)

// parseUUID is a thin wrapper used after validator has already confirmed the
// string is uuid4-shaped, checked anyway per .golangci.yml's errcheck rule.
func parseUUID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the finance surface. Every route is additionally gated to
// finance/super_admin by internal/rbac.GroupFinance -- see main.go.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/ledger", h.ledger)
	// "/ledger/export", not "/reports/payments.csv": the resource is the
	// ledger and this is a representation of it, so it belongs beside
	// GET /ledger rather than implying a reports subsystem that does not
	// exist. The format is announced by Content-Type and Content-Disposition,
	// which is where a client should be reading it from anyway.
	r.Get("/ledger/export", h.exportCSV)
	r.Get("/commission-rules", h.getCommissionRule)
	r.Put("/commission-rules", h.setCommissionRule)
	r.Post("/payouts/run", h.runPayoutBatch)
	r.Get("/payout-batches", h.listPayoutBatches)
	r.Get("/commission-rules/history", h.commissionHistory)
	r.Get("/refunds", h.listRefunds)
	r.Post("/refunds/{id}/decision", h.decideRefund)
	r.Post("/refunds", h.approveRefund)
	return r
}

type ledgerDTO struct {
	PaymentID       string    `json:"payment_id"`
	AppointmentID   string    `json:"appointment_id,omitempty"`
	DoctorID        string    `json:"doctor_id,omitempty"`
	PatientID       string    `json:"patient_id,omitempty"`
	AmountCents     int64     `json:"amount_cents"`
	CommissionCents int64     `json:"commission_cents"`
	Currency        string    `json:"currency"`
	Status          string    `json:"status"`
	Provider        string    `json:"provider,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func toDTO(e LedgerEntry) ledgerDTO {
	d := ledgerDTO{
		PaymentID: e.PaymentID.String(), AmountCents: e.AmountCents, CommissionCents: e.CommissionCents,
		Currency: e.Currency, Status: e.Status, Provider: e.Provider, OccurredAt: e.OccurredAt,
	}
	if e.AppointmentID != uuid.Nil {
		d.AppointmentID = e.AppointmentID.String()
	}
	if e.DoctorID != uuid.Nil {
		d.DoctorID = e.DoctorID.String()
	}
	if e.PatientID != uuid.Nil {
		d.PatientID = e.PatientID.String()
	}
	return d
}

func ledgerFilterFromRequest(r *http.Request) (filter LedgerFilter, page, perPage int, err error) {
	page, perPage, _ = httpx.Pagination(r)
	q := r.URL.Query()
	doctorID, _, err := httpx.QueryUUID(r, "doctor_id")
	if err != nil {
		return LedgerFilter{}, 0, 0, err
	}
	var from, to time.Time
	if raw := q.Get("from"); raw != "" {
		from, err = time.Parse("2006-01-02", raw)
		if err != nil {
			return LedgerFilter{}, 0, 0, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "from must be YYYY-MM-DD")
		}
	}
	if raw := q.Get("to"); raw != "" {
		to, err = time.Parse("2006-01-02", raw)
		if err != nil {
			return LedgerFilter{}, 0, 0, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to must be YYYY-MM-DD")
		}
	}
	return LedgerFilter{Status: q.Get("status"), DoctorID: doctorID, From: from, To: to, Page: page, PerPage: perPage}, page, perPage, nil
}

func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	f, page, perPage, err := ledgerFilterFromRequest(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	items, total, err := h.svc.Ledger(r.Context(), f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]ledgerDTO, len(items))
	for i := range items {
		dtos[i] = toDTO(items[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// exportCSV streams the payments ledger.
//
// It was previously three separate problems in twenty lines: ledgerFilter
// returns an empty WHERE when nothing is supplied, so a bare GET was a
// full-table sort-and-stream of every payment on the platform with patient_id
// and doctor_id on every row; the export wrote no audit entry, on a service
// whose whole purpose is that admin actions are recorded; and the stream error
// was discarded AFTER the 200, so a truncated financial report was
// indistinguishable from a complete one.
//
// The audit subsystem had already solved all three. This is the same shape:
// validate, bound, write the audit row, and only then start streaming.
func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request) {
	f, _, _, err := ledgerFilterFromRequest(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	rowCount, err := h.svc.PrepareExport(r.Context(), f)
	if err != nil {
		httpx.Error(w, r, exportError(err))
		return
	}

	// A bulk export of every payment, patient id included, is an auditable
	// action -- arguably the most interesting one, since it is what precedes
	// exfiltration. audit.Middleware cannot record it because it skips GETs,
	// so the handler stages it and Middleware persists it.
	audit.Stage(r.Context(), audit.Draft{
		Action:       "finance.ledger_exported",
		ResourceType: "payments_projection",
		NewValue: map[string]any{
			"from":      f.From.UTC().Format(time.RFC3339),
			"to":        f.To.UTC().Format(time.RFC3339),
			"status":    f.Status,
			"doctor_id": f.DoctorID.String(),
			"row_count": rowCount,
		},
	})

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="payments-report.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"payment_id", "appointment_id", "doctor_id", "patient_id", "amount_cents", "commission_cents", "currency", "status", "provider", "occurred_at"})
	emitErr := h.svc.ExportLedger(r.Context(), f, func(e LedgerEntry) error {
		// csvx.Row for the same reason the audit export uses it: status and
		// provider are strings this service does not author, and a financial
		// report is opened in Excel by definition.
		return cw.Write(csvx.Row([]string{
			e.PaymentID.String(), e.AppointmentID.String(), e.DoctorID.String(), e.PatientID.String(),
			strconv.FormatInt(e.AmountCents, 10), strconv.FormatInt(e.CommissionCents, 10),
			e.Currency, e.Status, e.Provider, e.OccurredAt.UTC().Format(time.RFC3339),
		}))
	})
	cw.Flush()

	if emitErr := errors.Join(emitErr, cw.Error()); emitErr != nil {
		// The 200 is already on the wire, so this cannot become an error
		// response -- but it must not become a successful one either. A
		// financial report that stops early looks exactly like a complete
		// report of a quieter period. Aborting the connection makes the client
		// see a failed transfer; platform/server's Recoverer treats
		// http.ErrAbortHandler as a deliberate abort rather than a crash.
		log := logger.FromContext(r.Context())
		log.Error().Err(emitErr).
			Int64("row_count", rowCount).
			Msg("finance ledger export failed mid-stream; aborting the response so a truncated CSV cannot look complete")
		panic(http.ErrAbortHandler)
	}
}

// exportError maps the export bounds onto the HTTP taxonomy.
func exportError(err error) error {
	switch {
	case errors.Is(err, ErrExportUnbounded), errors.Is(err, ErrExportWindowTooWide):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
	case errors.Is(err, ErrExportTooLarge):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, err.Error())
	default:
		return err
	}
}

func (h *Handler) getCommissionRule(w http.ResponseWriter, r *http.Request) {
	cfg, rule, err := h.svc.CurrentCommissionConfig(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, commissionConfigDTO(cfg, rule))
}

type commissionRuleRequest struct {
	DefaultPercent int             `json:"default_percent" validate:"omitempty,min=0,max=100"`
	BySpecialty    map[string]int  `json:"by_specialty"`
	EffectiveFrom  *time.Time      `json:"effective_from"`
	Value          json.RawMessage `json:"value"`
}

func (h *Handler) setCommissionRule(w http.ResponseWriter, r *http.Request) {
	var body commissionRuleRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	rule := CommissionRule{DefaultPercent: body.DefaultPercent, BySpecialty: body.BySpecialty}
	if len(body.Value) > 0 {
		parsed, err := DecodeCommissionValue(body.Value)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "value must be a commission rule document"))
			return
		}
		rule = parsed
	}
	if rule.DefaultPercent < 0 || rule.DefaultPercent > 100 {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "default commission percent must be 0-100"))
		return
	}
	actor, _ := adminusers.FromContext(r.Context())
	var effectiveFrom time.Time
	if body.EffectiveFrom != nil {
		effectiveFrom = *body.EffectiveFrom
	}

	cfg, err := h.svc.SetCommissionRule(r.Context(), middleware.MustPrincipal(r.Context()), actor.ID, rule, effectiveFrom)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, commissionConfigDTO(cfg, rule))
}

func commissionConfigDTO(cfg sysconfig.Config, rule CommissionRule) map[string]any {
	out := map[string]any{
		"key":            commissionRulesKey,
		"value":          EditorCommissionSet(rule),
		"version":        cfg.Version,
		"effective_from": cfg.EffectiveFrom,
		"created_at":     cfg.CreatedAt,
	}
	if cfg.UpdatedBy != uuid.Nil {
		out["updated_by"] = cfg.UpdatedBy.String()
	} else {
		out["updated_by"] = nil
	}
	return out
}

type payoutBatchRequest struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
}

func (h *Handler) runPayoutBatch(w http.ResponseWriter, r *http.Request) {
	var body payoutBatchRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &body); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	if body.To != nil {
		to = *body.To
	}
	if body.From != nil {
		from = *body.From
	}
	if !to.After(from) {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to must be after from"))
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.TriggerPayoutBatch(r.Context(), actor.ID, from, to); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{"status": "payout_batch_requested"}})
}

func (h *Handler) listPayoutBatches(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	items, total, err := h.svc.ListPayoutBatches(r.Context(), page, perPage)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]payoutBatchDTO, len(items))
	for i, b := range items {
		dtos[i] = toPayoutBatchDTO(b)
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) commissionHistory(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.CommissionHistory(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]map[string]any, len(items))
	for i, cfg := range items {
		rule, decodeErr := DecodeCommissionValue(cfg.Value)
		if decodeErr != nil {
			httpx.Error(w, r, decodeErr)
			return
		}
		dtos[i] = commissionConfigDTO(cfg, rule)
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) listRefunds(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	items, total, err := h.svc.ListRefunds(r.Context(), r.URL.Query().Get("status"), page, perPage)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]refundDTO, len(items))
	for i := range items {
		dtos[i] = toRefundDTO(items[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

type refundDecisionRequest struct {
	Decision string `json:"decision" validate:"required,oneof=approved rejected"`
	Note     string `json:"note" validate:"required,min=15,max=1000"`
}

func (h *Handler) decideRefund(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body refundDecisionRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())
	if err := h.svc.DecideRefund(r.Context(), actor.ID, id, body.Decision, body.Note); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{"status": "refund_" + body.Decision}})
}

type refundDTO struct {
	ID            string     `json:"id"`
	PaymentID     string     `json:"payment_id"`
	AppointmentID string     `json:"appointment_id,omitempty"`
	DisputeID     string     `json:"dispute_id,omitempty"`
	AmountCents   int64      `json:"amount_cents"`
	Currency      string     `json:"currency"`
	Reason        string     `json:"reason"`
	Status        string     `json:"status"`
	RequestedAt   time.Time  `json:"requested_at"`
	DecidedAt     *time.Time `json:"decided_at"`
	DecidedBy     string     `json:"decided_by,omitempty"`
}

func toRefundDTO(req RefundRequest) refundDTO {
	d := refundDTO{
		ID: req.ID.String(), PaymentID: req.PaymentID.String(), AmountCents: req.AmountCents,
		Currency: req.Currency, Reason: req.Reason, Status: req.Status, RequestedAt: req.RequestedAt,
		DecidedAt: req.DecidedAt,
	}
	if req.AppointmentID != uuid.Nil {
		d.AppointmentID = req.AppointmentID.String()
	}
	if req.DisputeID != uuid.Nil {
		d.DisputeID = req.DisputeID.String()
	}
	if req.DecidedBy != uuid.Nil {
		d.DecidedBy = req.DecidedBy.String()
	}
	return d
}

type payoutBatchDTO struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	DoctorCount int        `json:"doctor_count"`
	TotalCents  int64      `json:"total_cents"`
	Currency    string     `json:"currency"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func toPayoutBatchDTO(b PayoutBatch) payoutBatchDTO {
	return payoutBatchDTO{
		ID: b.ID.String(), Status: b.Status, DoctorCount: b.DoctorCount,
		TotalCents: b.TotalCents, Currency: b.Currency, CreatedAt: b.CreatedAt, CompletedAt: b.CompletedAt,
	}
}

type refundRequest struct {
	PaymentID   string `json:"payment_id" validate:"required,uuid4"`
	AmountCents int64  `json:"amount_cents" validate:"required,min=1"`
	Reason      string `json:"reason" validate:"required,max=1000"`
}

func (h *Handler) approveRefund(w http.ResponseWriter, r *http.Request) {
	var body refundRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	paymentID, err := parseUUID(body.PaymentID)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "payment_id must be a valid UUID"))
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.ApproveRefund(r.Context(), actor.ID, paymentID, body.AmountCents, body.Reason); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{"status": "refund_approved"}})
}
