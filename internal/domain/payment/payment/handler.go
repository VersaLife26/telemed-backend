package payment

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
)

// Handler is the HTTP surface. It parses, authorises at the transport level,
// delegates, and formats. There is no SQL here and no business rule; if a rule
// appears in this file it is in the wrong file.
type Handler struct {
	svc     *Service
	payouts *PayoutRunner
	invoice *InvoiceRenderer
	log     zerolog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, payouts *PayoutRunner, invoice *InvoiceRenderer, log zerolog.Logger) *Handler {
	return &Handler{svc: svc, payouts: payouts, invoice: invoice, log: log}
}

// Routes returns the authenticated /api/v1 payment routes. The caller mounts
// them behind RequireAuth; the webhook routes are mounted separately and
// deliberately outside it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	// Everything under here is financial. No proxy, no browser and no CDN
	// keeps a copy.
	r.Use(middleware.NoStore)

	r.Post("/intent", h.createIntent)
	r.Get("/", h.listMine)

	// Static prefixes are mounted before the {id} subtree for readability;
	// chi resolves a static segment ahead of a parameter regardless of
	// registration order, so "promo", "dialog", "methods" and "order" can
	// never be captured as a payment id.
	r.Mount("/promo", h.promoRoutes())
	r.Mount("/dialog", h.dialogRoutes())
	r.Mount("/methods", h.methodRoutes())
	r.Get("/order/{appointmentID}", h.orderSummary)

	r.Route("/{id}", func(r chi.Router) {
		r.Get("/", h.getPayment)
		r.Post("/refund", h.refund)
		r.Post("/confirm-pin", h.confirmPIN)
		r.Get("/invoice", h.invoicePDF)
		r.Get("/ledger", h.ledger)
	})
	return r
}

// PayoutRoutes returns the payout routes.
func (h *Handler) PayoutRoutes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.NoStore)
	r.With(middleware.RequireRole(middleware.RoleSuperAdmin)).Post("/run", h.runPayouts)
	r.Get("/", h.listPayouts)
	return r
}

// --- request bodies --------------------------------------------------------

type createIntentRequest struct {
	AppointmentID string `json:"appointment_id" validate:"required,uuid4"`
	Provider      string `json:"provider,omitempty" validate:"omitempty,oneof=stripe payhere dialog mock"`
	// Phone is required for carrier billing and ignored otherwise. It is
	// normalised, validated and never logged.
	Phone     string `json:"phone,omitempty" validate:"omitempty,sriphone"`
	ReturnURL string `json:"return_url,omitempty" validate:"omitempty,url"`
}

type refundRequest struct {
	// CancelledBy and StartAt let ops replay the policy for a cancellation
	// that happened outside the normal event flow. A patient calling this
	// endpoint gets the patient branch regardless of what they send.
	CancelledBy string     `json:"cancelled_by,omitempty" validate:"omitempty,oneof=doctor patient system"`
	NoShow      bool       `json:"no_show,omitempty"`
	StartAt     *time.Time `json:"start_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	// AmountCents overrides the policy. Ops only; a patient sending it gets 403.
	AmountCents int64  `json:"amount_cents,omitempty" validate:"omitempty,gte=0"`
	Reason      string `json:"reason,omitempty" validate:"omitempty,oneof=doctor_cancelled patient_cancelled_early patient_cancelled_late no_show admin_override duplicate"`
}

type confirmPINRequest struct {
	PIN string `json:"pin" validate:"required,min=4,max=8"`
}

// --- handlers --------------------------------------------------------------

func (h *Handler) createIntent(w http.ResponseWriter, r *http.Request) {
	var body createIntentRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	appointmentID, err := uuid.Parse(body.AppointmentID)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "appointment_id must be a valid UUID"))
		return
	}

	p := middleware.MustPrincipal(r.Context())
	phone := ""
	if body.Phone != "" {
		phone = httpx.NormalizePhone(body.Phone)
		if phone == "" {
			httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
				"phone must be a Sri Lankan mobile number"))
			return
		}
	}

	view, err := h.svc.CreateIntent(r.Context(), CreateIntentInput{
		AppointmentID: appointmentID,
		CallerID:      p.UserID,
		CallerIsOps:   p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleOps),
		Provider:      ProviderName(body.Provider),
		PatientPhone:  phone,
		ReturnURL:     body.ReturnURL,
	})
	if err != nil {
		h.fail(w, r, err, "create payment intent")
		return
	}
	httpx.Created(w, r, view)
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())
	out, err := h.svc.GetPayment(r.Context(), id, p.UserID, p.DoctorID,
		p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleOps, middleware.RoleSupport))
	if err != nil {
		h.fail(w, r, err, "get payment")
		return
	}

	refunds, err := h.svc.ListRefunds(r.Context(), out.ID)
	if err != nil {
		h.fail(w, r, err, "list refunds")
		return
	}
	httpx.OK(w, r, map[string]any{"payment": out, "refunds": refunds})
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	page, perPage, offset := httpx.Pagination(r)
	p := middleware.MustPrincipal(r.Context())

	items, total, err := h.svc.ListMyPayments(r.Context(), p.UserID, p.DoctorID, Page{Page: page, PerPage: perPage, Offset: offset})
	if err != nil {
		h.fail(w, r, err, "list payments")
		return
	}
	httpx.List(w, r, items, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body refundRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	p := middleware.MustPrincipal(r.Context())
	ops := p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance)

	in := RefundInput{
		PaymentID:   id,
		CallerID:    p.UserID,
		CallerIsOps: ops,
		Actor:       ActorPatient,
		Reason:      RefundReason(body.Reason),
	}
	if ops {
		// Only ops may assert who cancelled and when. A patient asserting
		// "the doctor cancelled" would upgrade their own refund to 100%.
		in.Actor = ParseCancelActor(body.CancelledBy)
		in.NoShow = body.NoShow
		in.AmountCents = body.AmountCents
		if body.CancelledAt != nil {
			in.CancelledAt = *body.CancelledAt
		}
		// StartAt belongs INSIDE this block, and its being outside was a
		// self-service refund. DecideRefund prices the patient branch purely
		// on notice = StartAt - CancelledAt (policy.go:105), so a patient
		// POSTing {"start_at":"2035-01-01T00:00:00Z"} cleared the
		// LateCancellationWindow by a decade and was handed 100% of their own
		// captured payment -- after attending the consultation, since nothing
		// on this path checks that the appointment was ever cancelled.
		// RefundInput's own doc comment already said a non-ops caller "may not
		// claim a start time"; only the brace disagreed.
		if body.StartAt != nil {
			in.StartAt = *body.StartAt
		}
	}

	refund, err := h.svc.Refund(r.Context(), in)
	if err != nil {
		h.fail(w, r, err, "refund payment")
		return
	}
	httpx.Created(w, r, refund)
}

func (h *Handler) confirmPIN(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body confirmPINRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	out, err := h.svc.ConfirmPIN(r.Context(), ConfirmPINInput{
		PaymentID:   id,
		CallerID:    p.UserID,
		CallerIsOps: p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleOps),
		PIN:         body.PIN,
	})
	if err != nil {
		h.fail(w, r, err, "confirm carrier billing PIN")
		return
	}
	httpx.OK(w, r, out)
}

func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())
	// The ledger is finance's view of the money, not the patient's receipt.
	if !p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance) {
		httpx.Error(w, r, httpx.ErrForbidden)
		return
	}
	entries, err := h.svc.Ledger(r.Context(), id)
	if err != nil {
		h.fail(w, r, err, "read ledger")
		return
	}
	httpx.OK(w, r, entries)
}

func (h *Handler) invoicePDF(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())
	pay, err := h.svc.GetPayment(r.Context(), id, p.UserID, p.DoctorID,
		p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleSupport))
	if err != nil {
		h.fail(w, r, err, "get payment for invoice")
		return
	}

	refunds, err := h.svc.ListRefunds(r.Context(), pay.ID)
	if err != nil {
		h.fail(w, r, err, "list refunds for invoice")
		return
	}
	rule, err := h.svc.RuleFor(r.Context(), pay)
	if err != nil {
		h.fail(w, r, err, "load commission rule for invoice")
		return
	}

	pdf, err := h.invoice.Render(Invoice{
		Payment: pay,
		Refunds: refunds,
		Rule:    rule,
		// The doctor's share is shown only to the doctor and to finance. A
		// patient's receipt shows what they paid, not what the platform kept.
		ShowSplit: p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance) ||
			(p.DoctorID != uuid.Nil && p.DoctorID == pay.DoctorID),
	})
	if err != nil {
		h.fail(w, r, err, "render invoice")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="invoice-`+pay.ID.String()+`.pdf"`)
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(pdf); err != nil {
		h.log.Warn().Err(err).Msg("invoice write failed after headers were sent")
	}
}

func (h *Handler) runPayouts(w http.ResponseWriter, r *http.Request) {
	summary, err := h.payouts.Run(r.Context())
	if errors.Is(err, ErrPayoutRunInProgress) {
		// 409, and say so plainly. An operator who is told "internal error"
		// while the nightly cron is mid-run will press the button again, and
		// again -- which is precisely the concurrency the lease exists to
		// prevent.
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"a payout run is already in progress; wait for it to finish").WithCause(err))
		return
	}
	if err != nil {
		h.fail(w, r, err, "run payouts")
		return
	}
	httpx.OK(w, r, summary)
}

func (h *Handler) listPayouts(w http.ResponseWriter, r *http.Request) {
	page, perPage, offset := httpx.Pagination(r)
	p := middleware.MustPrincipal(r.Context())

	items, total, err := h.svc.ListPayouts(r.Context(), p.DoctorID,
		p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance),
		Page{Page: page, PerPage: perPage, Offset: offset})
	if err != nil {
		h.fail(w, r, err, "list payouts")
		return
	}
	httpx.List(w, r, items, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// fail maps a domain error onto the platform error envelope. It is the only
// place in the service that decides an HTTP status, and it never lets a raw
// error string reach the client -- those can carry an account id or a provider
// message.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error, op string) {
	log := logger.FromContext(r.Context())

	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound.WithCause(err))
	case errors.Is(err, ErrRefundNotSelfService):
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeRefundNotAllowed,
			"a refund follows from cancelling the appointment; cancel the appointment and the refund is applied automatically under the cancellation policy").WithCause(err))
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, r, httpx.ErrForbidden.WithCause(err))
	case errors.Is(err, ErrPaymentNotReady):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodePaymentNotReady,
			"the appointment's payment has not been created yet; retry shortly").WithCause(err))
	case errors.Is(err, ErrNothingRefundable):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeRefundNotAllowed,
			"the cancellation policy returns nothing for this payment").WithCause(err))
	case errors.Is(err, ErrVersionConflict), errors.Is(err, ErrDuplicate):
		httpx.Error(w, r, httpx.ErrConflict.WithCause(err))
	case errors.Is(err, ErrUnsupported):
		httpx.Error(w, r, httpx.NewError(http.StatusNotImplemented, httpx.CodeProviderError,
			"the payment provider for this payment does not support that operation").WithCause(err))
	case errors.Is(err, ErrProviderRejected):
		httpx.Error(w, r, httpx.NewError(http.StatusPaymentRequired, httpx.CodePaymentRequired,
			"the payment provider declined the request").WithCause(err))

	// --- promotions ------------------------------------------------------
	//
	// Four codes rather than one UNPROCESSABLE, because the patient-facing
	// copy differs for every one and a client that can only see
	// UNPROCESSABLE has to fall back to the vaguest wording it has.
	case errors.Is(err, ErrPromoUnknown):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePromoInvalid,
			"we do not recognise that promo code").WithCause(err))
	case errors.Is(err, ErrPromoExpired):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePromoExpired,
			"that promo code is no longer valid").WithCause(err))
	case errors.Is(err, ErrPromoExhausted):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodePromoExhausted,
			"that promo code has been fully redeemed").WithCause(err))
	case errors.Is(err, ErrPromoNotApplicable):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePromoNotApplicable,
			"that promo code does not apply to this booking").WithCause(err))
	case errors.Is(err, ErrNoPromoApplied):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePromoNotApplicable,
			"there is no promo code on this booking to remove").WithCause(err))

	// --- carrier billing --------------------------------------------------
	//
	// PIN_INVALID is 422 and the others are 409, which is the difference that
	// matters to the app: 422 means "try again on this screen", 409 means
	// "this screen is over, go back".
	case errors.Is(err, ErrPINNotOutstanding):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodePINNotOutstanding,
			"there is no PIN waiting to be entered for this payment").WithCause(err))
	case errors.Is(err, ErrPINInvalid):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePINInvalid,
			"that PIN is not correct").WithCause(err))
	case errors.Is(err, ErrPINExpired):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodePINExpired,
			"that PIN has expired; request a new one").WithCause(err))
	case errors.Is(err, ErrPINAttemptsExceeded):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodePINAttemptsExceeded,
			"too many incorrect PIN attempts; this payment has been cancelled").WithCause(err))
	case errors.Is(err, ErrAlreadySettled):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"this consultation has already been paid for").WithCause(err))

	// --- saved payment methods --------------------------------------------
	case errors.Is(err, ErrVaultDisabled):
		httpx.Error(w, r, httpx.NewError(http.StatusNotImplemented, httpx.CodeProviderError,
			"saved payment methods are not available on this deployment").WithCause(err))
	case errors.Is(err, ErrSetupIncomplete):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodePaymentMethodError,
			"that card was not saved; the setup did not complete").WithCause(err))
	case errors.Is(err, ErrMethodNotOwned):
		httpx.Error(w, r, httpx.ErrForbidden.WithCause(err))
	case errors.Is(err, ErrProviderUnavailable):
		log.Error().Err(err).Str("op", op).Msg("payment provider unavailable")
		httpx.Error(w, r, httpx.NewError(http.StatusBadGateway, httpx.CodeProviderError,
			"the payment provider is not responding; please try again").WithCause(err))
	case errors.Is(err, ErrProviderNotConfigured):
		httpx.Error(w, r, httpx.NewError(http.StatusNotImplemented, httpx.CodeProviderError,
			"that payment method is not available on this server").WithCause(err))
	default:
		log.Error().Err(err).Str("op", op).Msg("payment request failed")
		httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
	}
}
