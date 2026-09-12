package notification

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Handler is HTTP only: it decodes requests, calls Service, and encodes
// responses. It never runs SQL and never applies a business rule beyond
// "which role may call this".
type Handler struct {
	svc  *Service
	repo *Repository
	log  zerolog.Logger
}

// NewHandler builds the notification HTTP handler.
func NewHandler(svc *Service, repo *Repository, log zerolog.Logger) *Handler {
	return &Handler{svc: svc, repo: repo, log: log}
}

// Routes mounts every /api/v1/notifications endpoint plus the delivery
// webhook. main.go mounts this under /api/v1/notifications and /webhooks
// respectively.
func (h *Handler) Routes(r chi.Router, auth *middleware.Authenticator) {
	r.Route("/notifications", func(r chi.Router) {
		r.Use(middleware.RequireAuth(auth))

		r.With(middleware.RequireRole(middleware.RoleService)).Post("/send", h.send)

		r.Get("/", h.list)
		// read-all is registered before the {id} subtree for readability only:
		// chi resolves the static segment ahead of the parameter regardless of
		// registration order, so "read-all" can never be captured as an id.
		r.Put("/read-all", h.markAllRead)
		r.Put("/{id}/read", h.markRead)

		r.Get("/preferences", h.getPreferences)
		r.Put("/preferences", h.putPreferences)

		r.Post("/devices", h.registerDevice)
		r.Delete("/devices/{id}", h.deleteDevice)
	})
}

// maxWebhookBody caps a delivery callback. Real payloads are a few hundred
// bytes of form or JSON.
//
// The Twilio branch calls r.ParseForm(), which without a MaxBytesReader falls
// back to net/http's 10 MB maxFormSize -- a 10 MB allocation per request, on
// an endpoint with no credential and no limiter, at whatever concurrency the
// caller chooses.
const maxWebhookBody = 64 * 1024

// WebhookRoutes mounts /webhooks/delivery/{provider}.
//
// The comment this replaces said webhooks are unauthenticated "because
// providers do not carry our bearer tokens", and mounted the route bare: no
// signature check, no allowlist, no rate limit, no body cap. The premise is
// wrong, and payment-service disproves it in the same tree -- every provider
// here lets the callback URL be configured, and a high-entropy token in that
// URL is how you authenticate a caller that cannot sign.
//
// What the route reaches is not cosmetic. ApplyDeliveryReceipt is
// `UPDATE notifications ... WHERE provider = $1 AND provider_message_id = $2`
// with no user scoping, and a provider_message_id is a Twilio SID or an FCM
// message id -- not a secret, and shorter than a UUID. Anyone who learned or
// guessed one could mark another user's notification delivered, destroying the
// delivery evidence, or write chosen text into notifications.last_error, which
// the retention job never prunes.
//
// secret is required: NewHandler refuses to build webhook routes without one
// (see cmd/server). rl is the rate limiter the caller wires up.
func (h *Handler) WebhookRoutes(r chi.Router, secret string, rl func(http.Handler) http.Handler) {
	if rl != nil {
		r.Use(rl)
	}
	r.Use(requireWebhookToken(secret, h.log))
	r.Post("/delivery/{provider}", h.deliveryWebhook)
}

// webhookTokenHeader is where a provider carries the shared secret. Every
// backend this service uses (Twilio status callbacks, Dialog DLRs, FCM, SES
// via SNS) allows an arbitrary callback URL, and most allow custom headers;
// the query parameter is the fallback for the ones that do not.
const webhookTokenHeader = "X-Telemed-Webhook-Token" //nolint:gosec // G101: a header NAME, not a credential

// requireWebhookToken is the shared-secret gate on the delivery callback.
//
// It is deliberately the ONLY mechanism rather than one of several: a single
// control that is actually enforced beats a menu of half-implemented
// per-provider signature checks. Per-provider signatures -- Twilio's
// X-Twilio-Signature in particular -- are the stronger control and are worth
// adding, but they need validating against a real provider payload, and a
// signature check written blind that rejects genuine callbacks silently loses
// every delivery receipt on the platform.
//
// Constant-time compare: the token is a fixed secret checked on an
// unauthenticated endpoint a caller may hit as often as the limiter allows,
// which is the textbook shape for a byte-at-a-time timing oracle.
func requireWebhookToken(secret string, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				// Unreachable in a booted service; a build-time mistake, not a
				// runtime state to fail open on.
				httpx.Error(w, r, httpx.ErrForbidden)
				return
			}
			presented := r.Header.Get(webhookTokenHeader)
			if presented == "" {
				presented = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
				log.Warn().
					Str("provider", chi.URLParam(r, "provider")).
					Str("ip", middleware.ClientIP(r)).
					Msg("rejected a delivery callback with a missing or wrong webhook token")
				httpx.Error(w, r, httpx.NewError(http.StatusUnauthorized, httpx.CodeSignatureInvalid,
					"delivery callbacks must carry the configured webhook token"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/notifications/send
// ---------------------------------------------------------------------------

type templateDataDTO struct {
	DoctorName       string `json:"doctor_name"`
	DateTime         string `json:"date_time"`
	ProposedDateTime string `json:"proposed_date_time"`
	FeeLKR           string `json:"fee_lkr"`
	AmountLKR        string `json:"amount_lkr"`
	Reason           string `json:"reason"`
	DownloadURL      string `json:"download_url"`
	ReceiptURL       string `json:"receipt_url"`
	JoinLink         string `json:"join_link"`
	Code             string `json:"code"`
	ExpiresInMinutes int    `json:"expires_in_minutes"`
	ApplicantEmail   string `json:"applicant_email"`
	Phone            string `json:"phone"`
	SLMCNumber       string `json:"slmc_number"`
	Specialty        string `json:"specialty"`
	PortalURL        string `json:"portal_url"`
}

// toDomain converts the wire DTO to the render input. The conversion is a
// direct struct conversion, which is deliberate: it will stop compiling the
// moment TemplateData and templateDataDTO stop having exactly the same fields,
// forcing whoever adds a render field to decide explicitly whether a client is
// allowed to supply it.
func (d templateDataDTO) toDomain() TemplateData {
	return TemplateData(d)
}

type sendRequest struct {
	UserID      uuid.UUID       `json:"user_id" validate:"required"`
	TemplateKey string          `json:"template_key" validate:"required"`
	Data        templateDataDTO `json:"data"`
	Phone       string          `json:"phone" validate:"omitempty,sriphone"`
	Email       string          `json:"email" validate:"omitempty,email"`
	Locale      string          `json:"locale" validate:"omitempty,oneof=en si ta"`
	Channels    []string        `json:"channels" validate:"omitempty,dive,oneof=sms push email in_app"`
	DedupeKey   string          `json:"dedupe_key" validate:"required"`
	Immediate   bool            `json:"immediate"`
}

type sendOutcomeDTO struct {
	NotificationID uuid.UUID `json:"notification_id,omitempty"`
	Channel        string    `json:"channel"`
	Status         string    `json:"status"`
	Created        bool      `json:"created"`
}

// send is the internal, service-role-only synchronous notification API.
// It is the path OTP codes and any other latency-sensitive send takes:
// Notify enqueues (respecting preferences/quiet hours/dedupe exactly like
// the event-driven path), and when Immediate is set, this handler follows
// up with DispatchOne per channel so the caller gets a real delivery result
// rather than "we queued it, ask again later".
func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	channels := make([]Channel, 0, len(req.Channels))
	for _, c := range req.Channels {
		channels = append(channels, Channel(c))
	}

	outcomes, err := h.svc.Notify(r.Context(), NotifyRequest{
		UserID:        req.UserID,
		TemplateKey:   TemplateKey(req.TemplateKey),
		Data:          req.Data.toDomain(),
		Phone:         req.Phone,
		Email:         req.Email,
		DedupeKeyBase: req.DedupeKey,
		LocaleHint:    Locale(req.Locale),
		Channels:      channels,
		Immediate:     req.Immediate,
	})
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, err.Error()))
		return
	}

	out := make([]sendOutcomeDTO, 0, len(outcomes))
	for _, o := range outcomes {
		if req.Immediate && o.Created && o.Status == StatusSending {
			if err := h.svc.DispatchOne(r.Context(), o.NotificationID); err != nil {
				h.log.Error().Err(err).Str("notification_id", o.NotificationID.String()).Msg("immediate dispatch failed")
			}
			if n, err := h.repo.GetByID(r.Context(), h.repo.Pool(), o.NotificationID, uuid.Nil); err == nil {
				o.Status = n.Status
			}
		}
		out = append(out, sendOutcomeDTO{
			NotificationID: o.NotificationID, Channel: string(o.Channel),
			Status: string(o.Status), Created: o.Created,
		})
	}
	httpx.Created(w, r, out)
}

// ---------------------------------------------------------------------------
// GET /api/v1/notifications
// ---------------------------------------------------------------------------

type notificationDTO struct {
	ID          uuid.UUID  `json:"id"`
	Channel     string     `json:"channel"`
	TemplateKey string     `json:"template_key"`
	Locale      string     `json:"locale"`
	Subject     string     `json:"subject,omitempty"`
	Body        string     `json:"body"`
	Status      string     `json:"status"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	ReadAt      *time.Time `json:"read_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toNotificationDTO(n Notification) notificationDTO {
	return notificationDTO{
		ID: n.ID, Channel: string(n.Channel), TemplateKey: string(n.TemplateKey), Locale: string(n.Locale),
		Subject: n.Subject, Body: n.Body, Status: string(n.Status),
		SentAt: n.SentAt, DeliveredAt: n.DeliveredAt, ReadAt: n.ReadAt, CreatedAt: n.CreatedAt,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	page, perPage, offset := httpx.Pagination(r)

	rows, total, err := h.repo.ListForUser(r.Context(), h.repo.Pool(), p.UserID, offset, perPage)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]notificationDTO, 0, len(rows))
	for i := range rows {
		out = append(out, toNotificationDTO(rows[i]))
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// ---------------------------------------------------------------------------
// PUT /api/v1/notifications/{id}/read
// ---------------------------------------------------------------------------

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.repo.MarkRead(r.Context(), h.repo.Pool(), id, p.UserID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// ---------------------------------------------------------------------------
// PUT /api/v1/notifications/read-all
// ---------------------------------------------------------------------------

// markAllReadResult reports how many notifications the call actually changed,
// so a client can decide whether to refresh its list or leave it alone. It is
// a count, not the list itself: the notification list is paginated and a bulk
// mark can cross thousands of rows.
type markAllReadResult struct {
	Updated int64 `json:"updated"`
}

// markAllRead clears the whole unread badge in one call.
//
// It is PUT rather than POST because it is idempotent in the HTTP sense: the
// second call leaves the resource in exactly the state the first one did, and
// a client that retries after a dropped response is not at risk of doing
// anything twice. It also matches PUT /{id}/read, so the two mark-as-read
// verbs on this service are the same verb.
func (h *Handler) markAllRead(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	n, err := h.repo.MarkAllRead(r.Context(), h.repo.Pool(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, markAllReadResult{Updated: n})
}

// ---------------------------------------------------------------------------
// GET/PUT /api/v1/notifications/preferences
// ---------------------------------------------------------------------------

type preferencesDTO struct {
	SMSEnabled      bool   `json:"sms_enabled"`
	PushEnabled     bool   `json:"push_enabled"`
	EmailEnabled    bool   `json:"email_enabled"`
	InAppEnabled    bool   `json:"in_app_enabled"`
	Locale          string `json:"locale"`
	QuietHoursStart string `json:"quiet_hours_start,omitempty"` // "HH:MM", 24h
	QuietHoursEnd   string `json:"quiet_hours_end,omitempty"`
	Timezone        string `json:"timezone"`
}

func toPreferencesDTO(p Preferences) preferencesDTO {
	return preferencesDTO{
		SMSEnabled: p.SMSEnabled, PushEnabled: p.PushEnabled, EmailEnabled: p.EmailEnabled, InAppEnabled: p.InAppEnabled,
		Locale: string(p.Locale), QuietHoursStart: formatClock(p.QuietHoursStart), QuietHoursEnd: formatClock(p.QuietHoursEnd),
		Timezone: p.Timezone,
	}
}

func (h *Handler) getPreferences(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	prefs, found, err := h.repo.GetPreferences(r.Context(), h.repo.Pool(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !found {
		prefs = DefaultPreferences(p.UserID)
	}
	httpx.OK(w, r, toPreferencesDTO(prefs))
}

type putPreferencesRequest struct {
	SMSEnabled      bool   `json:"sms_enabled"`
	PushEnabled     bool   `json:"push_enabled"`
	EmailEnabled    bool   `json:"email_enabled"`
	InAppEnabled    bool   `json:"in_app_enabled"`
	Locale          string `json:"locale" validate:"required,oneof=en si ta"`
	QuietHoursStart string `json:"quiet_hours_start" validate:"omitempty"`
	QuietHoursEnd   string `json:"quiet_hours_end" validate:"omitempty"`
	Timezone        string `json:"timezone" validate:"required"`
}

func (h *Handler) putPreferences(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req putPreferencesRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if _, err := time.LoadLocation(req.Timezone); err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "timezone is not a valid IANA zone"))
		return
	}

	start, err := parseClock(req.QuietHoursStart)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "quiet_hours_start must be HH:MM"))
		return
	}
	end, err := parseClock(req.QuietHoursEnd)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "quiet_hours_end must be HH:MM"))
		return
	}
	if (start == nil) != (end == nil) {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "quiet_hours_start and quiet_hours_end must be set together"))
		return
	}

	prefs := Preferences{
		UserID: p.UserID, SMSEnabled: req.SMSEnabled, PushEnabled: req.PushEnabled,
		EmailEnabled: req.EmailEnabled, InAppEnabled: req.InAppEnabled, Locale: Locale(req.Locale),
		QuietHoursStart: start, QuietHoursEnd: end, Timezone: req.Timezone,
	}
	if err := h.repo.UpsertPreferences(r.Context(), h.repo.Pool(), prefs); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toPreferencesDTO(prefs))
}

// formatClock renders minutes-since-midnight as "HH:MM"; nil renders "".
func formatClock(d *time.Duration) string {
	if d == nil {
		return ""
	}
	total := int(*d / time.Minute)
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// parseClock parses "HH:MM" into minutes-since-midnight; "" returns nil.
func parseClock(s string) (*time.Duration, error) {
	if s == "" {
		return nil, nil
	}
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return nil, err
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return nil, fmt.Errorf("notification: %q is out of range", s)
	}
	d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	return &d, nil
}

// ---------------------------------------------------------------------------
// POST /api/v1/notifications/devices, DELETE .../devices/{id}
// ---------------------------------------------------------------------------

type registerDeviceRequest struct {
	// max=512 is well above every real push token (an FCM registration token
	// is ~163 characters, an APNs one 64 hex) and stops an unbounded string
	// becoming an unbounded row.
	Token    string `json:"token" validate:"required,max=512"`
	Platform string `json:"platform" validate:"required,oneof=ios android web"`
}

type deviceDTO struct {
	ID         uuid.UUID `json:"id"`
	Platform   string    `json:"platform"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func (h *Handler) registerDevice(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req registerDeviceRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Cap the number of registered handsets. Registration had no bound, and
	// every push to a user fans out as one outbound provider call per active
	// token -- so N junk registrations made every subsequent notification to
	// that account cost N provider calls. The count is taken before the
	// upsert; re-registering an existing token is a no-op on the count
	// because the conflict target is (user_id, token), so a legitimate client
	// refreshing its own token is never refused.
	n, err := h.repo.CountActiveDeviceTokens(r.Context(), h.repo.Pool(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if n >= MaxActiveDeviceTokens && !h.deviceTokenKnown(r, p.UserID, req.Token) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			fmt.Sprintf("at most %d devices may be registered; remove one first", MaxActiveDeviceTokens)))
		return
	}

	id, err := h.repo.UpsertDeviceToken(r.Context(), h.repo.Pool(), DeviceToken{
		UserID: p.UserID, Token: req.Token, Platform: Platform(req.Platform),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, deviceDTO{ID: id, Platform: req.Platform, LastSeenAt: time.Now().UTC()})
}

// deviceTokenKnown reports whether this exact token is already registered to
// this user, so a client re-registering at the cap is refreshing rather than
// adding.
func (h *Handler) deviceTokenKnown(r *http.Request, userID uuid.UUID, token string) bool {
	existing, err := h.repo.ListActiveDeviceTokens(r.Context(), h.repo.Pool(), userID)
	if err != nil {
		return false
	}
	for i := range existing {
		if existing[i].Token == token {
			return true
		}
	}
	return false
}

func (h *Handler) deleteDevice(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.repo.InvalidateDeviceToken(r.Context(), h.repo.Pool(), id, p.UserID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// ---------------------------------------------------------------------------
// POST /webhooks/delivery/{provider}
// ---------------------------------------------------------------------------

// deliveryWebhook applies a provider's delivery/bounce callback to the
// matching notification, found by provider_message_id. Each provider's
// webhook payload shape is genuinely different (Twilio: form-encoded status
// callbacks; Dialog: JSON DLRs; SES: SNS-wrapped bounce notifications) and
// none of them are specified in the source documentation beyond "there is a
// webhook" -- this handler accepts either form or JSON bodies and looks for
// the field name variants each provider is documented to use. Flagged in
// the build report as needing validation against live provider payloads.
func (h *Handler) deliveryWebhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")

	// Cap the body before anything reads it. ParseForm below would otherwise
	// allocate up to net/http's 10 MB default.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)

	messageID, delivered, failReason, err := parseDeliveryWebhook(provider, r)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	}
	if messageID == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "could not find a message id in the webhook payload"))
		return
	}

	if err := h.repo.ApplyDeliveryReceipt(r.Context(), providerName(provider), messageID, delivered, failReason); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The provider may retry a webhook for a message we have since
			// pruned or never tracked (e.g. a test send); acknowledging with
			// 200 avoids an infinite provider-side retry loop for a
			// permanently unmatchable event.
			httpx.OK(w, r, map[string]string{"status": "ignored"})
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]string{"status": "applied"})
}

// providerName maps the URL segment to the provider name stored on
// notifications.provider.
func providerName(urlSegment string) string {
	switch strings.ToLower(urlSegment) {
	case "twilio":
		return "direct-twilio"
	case "dialog":
		return "direct-dialog"
	case "smtp", "ses":
		return "direct-smtp"
	case "fcm":
		return "direct-fcm"
	default:
		return urlSegment
	}
}

func parseDeliveryWebhook(provider string, r *http.Request) (messageID string, delivered bool, failReason string, err error) {
	ct := r.Header.Get("Content-Type")

	switch strings.ToLower(provider) {
	case "twilio":
		if err := r.ParseForm(); err != nil {
			return "", false, "", fmt.Errorf("parse form: %w", err)
		}
		messageID = r.PostForm.Get("MessageSid")
		status := strings.ToLower(r.PostForm.Get("MessageStatus"))
		delivered = status == "delivered"
		if status == "failed" || status == "undelivered" {
			failReason = "twilio status: " + status
		}
		return messageID, delivered, failReason, nil

	default:
		if !strings.HasPrefix(ct, "application/json") && ct != "" {
			return "", false, "", fmt.Errorf("unsupported content type %q for provider %q", ct, provider)
		}
		var body struct {
			MessageID string `json:"message_id"`
			Status    string `json:"status"`
			Reason    string `json:"reason"`
		}
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if readErr != nil {
			return "", false, "", fmt.Errorf("read body: %w", readErr)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				return "", false, "", fmt.Errorf("decode json: %w", err)
			}
		}
		status := strings.ToLower(body.Status)
		delivered = status == "delivered" || status == "success"
		if status == "failed" || status == "bounced" || status == "undelivered" {
			// Scrubbed and capped: this string is persisted verbatim into
			// notifications.last_error, which the retention job never prunes,
			// and a provider's own bounce text routinely quotes the
			// destination ("<patient@example.com>: Recipient address
			// rejected"). It is caller-supplied here besides.
			failReason = ScrubContact(body.Reason)
			if failReason == "" {
				failReason = "provider status: " + status
			}
		}
		return body.MessageID, delivered, failReason, nil
	}
}
