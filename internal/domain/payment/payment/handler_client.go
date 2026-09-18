package payment

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// The HTTP surface the patient app calls and this service did not have:
// promotional codes, the Dialog carrier-billing PIN pair, and saved cards.
//
// Same rules as handler.go: parse, authorise at the transport level, delegate,
// format. No SQL and no business rule below this line.

// promoRoutes mounts /api/v1/payments/promo.
func (h *Handler) promoRoutes() chi.Router {
	r := chi.NewRouter()
	r.Post("/apply", h.applyPromo)
	r.Post("/remove", h.removePromo)

	// A promo code is a standing instruction to give money away, so issuing
	// one is finance's decision and not a doctor's or a patient's.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireRole(middleware.RoleSuperAdmin, middleware.RoleFinance))
		r.Post("/codes", h.createPromoCode)
		r.Get("/codes", h.listPromoCodes)
		r.Delete("/codes/{code}", h.deactivatePromoCode)
	})
	return r
}

// dialogRoutes mounts /api/v1/payments/dialog, the carrier-billing pair.
//
// These are appointment-keyed rather than payment-keyed because an
// appointment id is the only identifier the patient's app holds when it opens
// the carrier-billing screen -- the payment row was created by an event, and
// the client has never seen its id. The payment-keyed
// POST /payments/{id}/confirm-pin remains, unchanged, for support tooling and
// for a client that already has the payment in hand; both call the same
// service method.
func (h *Handler) dialogRoutes() chi.Router {
	r := chi.NewRouter()
	r.Post("/request-pin", h.requestDialogPIN)
	r.Post("/confirm", h.confirmDialogPIN)
	return r
}

// methodRoutes mounts /api/v1/payments/methods, the saved-card surface.
func (h *Handler) methodRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.listMethods)
	r.Post("/", h.confirmCardSetup)
	r.Post("/setup-intent", h.startCardSetup)
	r.Delete("/{methodID}", h.forgetMethod)
	r.Put("/{methodID}/default", h.setDefaultMethod)
	return r
}

// --- promotions -------------------------------------------------------------

type applyPromoRequest struct {
	AppointmentID string `json:"appointment_id" validate:"required,uuid4"`
	Code          string `json:"code" validate:"required,min=3,max=32"`
}

type removePromoRequest struct {
	AppointmentID string `json:"appointment_id" validate:"required,uuid4"`
}

func (h *Handler) applyPromo(w http.ResponseWriter, r *http.Request) {
	var body applyPromoRequest
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
	summary, err := h.svc.ApplyPromo(r.Context(), ApplyPromoInput{
		AppointmentID: appointmentID,
		CallerID:      p.UserID,
		CallerIsOps:   p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleOps),
		Code:          body.Code,
	})
	if err != nil {
		h.fail(w, r, err, "apply promo code")
		return
	}
	httpx.OK(w, r, summary)
}

func (h *Handler) removePromo(w http.ResponseWriter, r *http.Request) {
	var body removePromoRequest
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
	summary, err := h.svc.RemovePromo(r.Context(), RemovePromoInput{
		AppointmentID: appointmentID,
		CallerID:      p.UserID,
		CallerIsOps:   p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleOps),
	})
	if err != nil {
		h.fail(w, r, err, "remove promo code")
		return
	}
	httpx.OK(w, r, summary)
}

// orderSummary is the server's arithmetic for the payment screen.
//
// It exists so the client never subtracts a discount itself. A total computed
// on the handset is a total an attacker controls, and it is also a total that
// silently disagrees with the charge the moment a reservation lapses.
func (h *Handler) orderSummary(w http.ResponseWriter, r *http.Request) {
	appointmentID, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	summary, err := h.svc.OrderSummaryFor(r.Context(), appointmentID, p.UserID,
		p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance, middleware.RoleOps, middleware.RoleSupport))
	if err != nil {
		h.fail(w, r, err, "read order summary")
		return
	}
	httpx.OK(w, r, summary)
}

type createPromoCodeRequest struct {
	Code             string     `json:"code" validate:"required,min=3,max=32"`
	Description      string     `json:"description,omitempty" validate:"omitempty,max=200"`
	DiscountType     string     `json:"discount_type" validate:"required,oneof=percent fixed"`
	PercentBps       int        `json:"percent_bps,omitempty" validate:"omitempty,gte=0,lte=10000"`
	AmountOffCents   int64      `json:"amount_off_cents,omitempty" validate:"omitempty,gte=0"`
	MaxDiscountCents int64      `json:"max_discount_cents,omitempty" validate:"omitempty,gte=0"`
	MinAmountCents   int64      `json:"min_amount_cents,omitempty" validate:"omitempty,gte=0"`
	Currency         string     `json:"currency,omitempty" validate:"omitempty,len=3"`
	ValidFrom        *time.Time `json:"valid_from,omitempty"`
	ValidUntil       *time.Time `json:"valid_until,omitempty"`
	MaxRedemptions   *int       `json:"max_redemptions,omitempty" validate:"omitempty,gt=0"`
	MaxPerUser       int        `json:"max_per_user,omitempty" validate:"omitempty,gt=0"`
}

func (h *Handler) createPromoCode(w http.ResponseWriter, r *http.Request) {
	var body createPromoCodeRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	code, err := h.svc.CreatePromoCode(r.Context(), CreatePromoCodeInput{
		Code:             body.Code,
		Description:      body.Description,
		DiscountType:     PromoDiscountType(body.DiscountType),
		PercentBps:       body.PercentBps,
		AmountOffCents:   body.AmountOffCents,
		MaxDiscountCents: body.MaxDiscountCents,
		MinAmountCents:   body.MinAmountCents,
		Currency:         body.Currency,
		ValidFrom:        body.ValidFrom,
		ValidUntil:       body.ValidUntil,
		MaxRedemptions:   body.MaxRedemptions,
		MaxPerUser:       body.MaxPerUser,
	})
	if err != nil {
		h.fail(w, r, err, "create promo code")
		return
	}
	httpx.Created(w, r, code)
}

func (h *Handler) ListPromoCodes(w http.ResponseWriter, r *http.Request) {
	h.listPromoCodes(w, r)
}

func (h *Handler) CreatePromoCode(w http.ResponseWriter, r *http.Request) {
	h.createPromoCode(w, r)
}

func (h *Handler) DeactivatePromoCode(w http.ResponseWriter, r *http.Request) {
	h.deactivatePromoCode(w, r)
}

func (h *Handler) listPromoCodes(w http.ResponseWriter, r *http.Request) {
	page, perPage, offset := httpx.Pagination(r)
	includeInactive := r.URL.Query().Get("include_inactive") == "true"

	items, total, err := h.svc.ListPromoCodes(r.Context(), includeInactive,
		Page{Page: page, PerPage: perPage, Offset: offset})
	if err != nil {
		h.fail(w, r, err, "list promo codes")
		return
	}
	httpx.List(w, r, items, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) deactivatePromoCode(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeactivatePromoCode(r.Context(), chi.URLParam(r, "code")); err != nil {
		h.fail(w, r, err, "deactivate promo code")
		return
	}
	httpx.NoContent(w, r)
}

// --- carrier billing --------------------------------------------------------

type requestPINRequest struct {
	AppointmentID string `json:"appointment_id" validate:"required,uuid4"`
	// Phone is required: this service does not hold the patient's number,
	// user-service does, and fetching it on the money path would put an
	// outage in user-service between a patient and their consultation.
	Phone string `json:"phone" validate:"required,sriphone"`
}

type confirmDialogPINRequest struct {
	AppointmentID string `json:"appointment_id" validate:"required,uuid4"`
	PIN           string `json:"pin" validate:"required,min=4,max=8"`
}

func (h *Handler) requestDialogPIN(w http.ResponseWriter, r *http.Request) {
	var body requestPINRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	appointmentID, err := uuid.Parse(body.AppointmentID)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "appointment_id must be a valid UUID"))
		return
	}
	phone := httpx.NormalizePhone(body.Phone)
	if phone == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"phone must be a Sri Lankan mobile number"))
		return
	}

	p := middleware.MustPrincipal(r.Context())
	challenge, err := h.svc.RequestPIN(r.Context(), RequestPINInput{
		AppointmentID: appointmentID,
		CallerID:      p.UserID,
		CallerIsOps:   p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleOps),
		Phone:         phone,
	})
	if err != nil {
		h.fail(w, r, err, "request carrier billing PIN")
		return
	}
	httpx.Created(w, r, challenge)
}

func (h *Handler) confirmDialogPIN(w http.ResponseWriter, r *http.Request) {
	var body confirmDialogPINRequest
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
	ops := p.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleOps)

	// The appointment is resolved to its payment here rather than in the
	// service, so ConfirmPIN has exactly one signature and one code path
	// whichever endpoint reached it.
	pay, err := h.svc.PaymentForAppointment(r.Context(), appointmentID, p.UserID, ops)
	if err != nil {
		h.fail(w, r, err, "resolve appointment payment")
		return
	}

	out, err := h.svc.ConfirmPIN(r.Context(), ConfirmPINInput{
		PaymentID:   pay.ID,
		CallerID:    p.UserID,
		CallerIsOps: ops,
		PIN:         body.PIN,
	})
	if err != nil {
		h.fail(w, r, err, "confirm carrier billing PIN")
		return
	}
	httpx.OK(w, r, out)
}

// --- saved payment methods --------------------------------------------------

type confirmSetupRequest struct {
	SetupIntentID string `json:"setup_intent_id" validate:"required,max=255"`
}

func (h *Handler) startCardSetup(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	view, err := h.svc.StartCardSetup(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err, "start card setup")
		return
	}
	httpx.Created(w, r, view)
}

func (h *Handler) confirmCardSetup(w http.ResponseWriter, r *http.Request) {
	var body confirmSetupRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	method, err := h.svc.ConfirmCardSetup(r.Context(), p.UserID, body.SetupIntentID)
	if err != nil {
		h.fail(w, r, err, "confirm card setup")
		return
	}
	httpx.Created(w, r, toMethodDTO(method, h.svc.Now()))
}

func (h *Handler) listMethods(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	methods, err := h.svc.ListCards(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err, "list payment methods")
		return
	}

	now := h.svc.Now()
	out := make([]methodDTO, 0, len(methods))
	// Indexed rather than ranged by value: PaymentMethod is a 216-byte struct
	// and this loop runs on every render of the saved-cards screen.
	for i := range methods {
		out = append(out, toMethodDTO(methods[i], now))
	}
	httpx.List(w, r, out, httpx.Meta{Page: 1, PerPage: len(out), Total: int64(len(out))})
}

func (h *Handler) forgetMethod(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "methodID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	if err := h.svc.ForgetCard(r.Context(), p.UserID, id); err != nil {
		h.fail(w, r, err, "forget payment method")
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setDefaultMethod(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "methodID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	method, err := h.svc.SetDefaultCard(r.Context(), p.UserID, id)
	if err != nil {
		h.fail(w, r, err, "set default payment method")
		return
	}
	httpx.OK(w, r, toMethodDTO(method, h.svc.Now()))
}

// methodDTO is the wire shape of a saved card.
//
// It is a separate type from PaymentMethod on purpose. PaymentMethod carries
// the provider token and the provider customer id, and both are credentials
// against the rail: a client holding one could act on the card outside our
// authorisation. A DTO with no field for them cannot leak them, however the
// struct above is later edited.
type methodDTO struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	Brand     string    `json:"brand,omitempty"`
	Last4     string    `json:"last4,omitempty"`
	ExpMonth  int       `json:"exp_month,omitempty"`
	ExpYear   int       `json:"exp_year,omitempty"`
	IsDefault bool      `json:"is_default"`
	Expired   bool      `json:"expired"`
	CreatedAt time.Time `json:"created_at"`
}

func toMethodDTO(m PaymentMethod, now time.Time) methodDTO {
	return methodDTO{
		ID:        m.ID,
		Type:      m.MethodType,
		Brand:     m.Brand,
		Last4:     m.Last4,
		ExpMonth:  m.ExpMonth,
		ExpYear:   m.ExpYear,
		IsDefault: m.IsDefault,
		Expired:   m.Expired(now),
		CreatedAt: m.CreatedAt,
	}
}
