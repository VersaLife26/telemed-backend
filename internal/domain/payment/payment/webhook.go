package payment

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
)

// Webhook endpoints.
//
// These routes are the only ones in the service that are not behind JWT
// authentication, and that is correct: Stripe does not hold a Keycloak token.
// They authenticate by signature instead, which is a stronger claim than a
// bearer token would be -- the signature covers the body, so it proves not just
// who is calling but exactly what they said.
//
// Being unauthenticated at the JWT layer makes three other controls mandatory:
//
//	rate limiting     an unauthenticated endpoint is a free DoS target
//	body size cap     the signature is computed over the whole body, so an
//	                  unbounded body is an unbounded allocation before we can
//	                  reject it
//	no body logging   a webhook body carries card metadata and, for a rejected
//	                  delivery, an attacker's chosen bytes
//
// All three are applied below rather than left to the deployment.

// MaxWebhookBody caps a webhook body at 256 KiB. The largest legitimate Stripe
// event is a few tens of kilobytes; PayHere's form post is under one.
const MaxWebhookBody int64 = 256 << 10

// WebhookHandler serves the provider callback endpoints.
type WebhookHandler struct {
	svc *Service
	log zerolog.Logger

	dialogCIDRs []string
}

// NewWebhookHandler builds the handler.
func NewWebhookHandler(svc *Service, log zerolog.Logger) *WebhookHandler {
	return &WebhookHandler{svc: svc, log: log}
}

// WithDialogAllowlist restricts /webhooks/dialog to the carrier's own egress
// ranges.
//
// This is the compensating control the design documents promised and the code
// did not have. Ideamart signs nothing natively; the provider's own comment and
// the boot log both asserted that when signing could not be arranged the
// endpoint fell back to "IP-allowlist-only protection", and there was no IP
// allowlist anywhere in this service. The fallback was a sentence.
//
// middleware.IPAllowlist is deliberately NOT used here. It treats an empty list
// as "not configured" and calls next, which is the right default for the admin
// surface -- where failing closed on a missing variable locks staff out of a
// running platform -- and exactly the wrong one for an unauthenticated endpoint
// that marks consultations paid. This allowlist fails CLOSED, and Settings.
// Validate refuses to boot the Dialog rail without one, so the empty case is a
// misconfiguration rather than a silent permit.
func (h *WebhookHandler) WithDialogAllowlist(cidrs []string) *WebhookHandler {
	h.dialogCIDRs = cidrs
	return h
}

// dialogAllowlist is the fail-closed CIDR gate described above.
//
// The client address comes from middleware.ClientIPFrom, which resolves
// X-Forwarded-For against TRUSTED_PROXIES rather than believing it -- a
// spoofable client IP would make the allowlist a formality. An unresolvable
// address is refused, not admitted.
func (h *WebhookHandler) dialogAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func(ip, reason string) {
			h.log.Warn().Str("provider", string(ProviderDialog)).
				Str("ip", ip).Str("reason", reason).
				Msg("rejected a Dialog callback from outside the carrier allowlist")
			httpx.Error(w, r, httpx.ErrForbidden)
		}

		if len(h.dialogCIDRs) == 0 {
			deny("", "DIALOG_WEBHOOK_CIDRS is not configured")
			return
		}
		ip := middleware.ClientIPFrom(r)
		if ip == "" {
			deny("", "the client address could not be resolved")
			return
		}
		ok, err := middleware.CIDRContainsAny(ip, h.dialogCIDRs)
		if err != nil || !ok {
			deny(ip, "not in DIALOG_WEBHOOK_CIDRS")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Routes mounts /webhooks/{provider}. The rate limiter is keyed on the client
// IP and shared across replicas through Redis, so an attacker cannot multiply
// their allowance by the number of pods.
func (h *WebhookHandler) Routes(c cache.Cache) chi.Router {
	r := chi.NewRouter()
	if c != nil {
		r.Use(middleware.RateLimit(c, middleware.RateLimitConfig{
			Name: "webhooks",
			// Generous enough for a burst of retries after an outage, tight
			// enough that a flood is cheap to shed.
			Requests: 600,
			Window:   time.Minute,
			// The only throttle in front of an anonymous, DB-writing,
			// 256 KiB-per-request endpoint. A Redis blip must not silently
			// remove it (F24). Every provider retries on 503.
			FailClosed: true,
		}, h.log))
	}
	r.Post("/stripe", h.handle(ProviderStripe))
	r.Post("/payhere", h.handle(ProviderPayHere))
	// Dialog alone carries the network gate: Stripe and PayHere sign their
	// callbacks with a secret we hold, and pinning them to a provider's
	// published egress ranges would trade a strong control for a brittle one.
	r.With(h.dialogAllowlist).Post("/dialog", h.handle(ProviderDialog))
	r.Post("/mock", h.handle(ProviderMock))
	return r
}

func (h *WebhookHandler) handle(provider ProviderName) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBody))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				h.log.Warn().Str("provider", string(provider)).Msg("webhook body exceeded the size cap")
				httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest,
					"webhook body too large"))
				return
			}
			httpx.Error(w, r, httpx.ErrBadRequest.WithCause(err))
			return
		}

		res, err := h.svc.HandleWebhook(r.Context(), provider, r.Header, body)
		if err != nil {
			h.respondError(w, r, provider, err)
			return
		}

		// The log line names the event and the outcome and nothing else. The
		// body lives in webhook_events, which is access-controlled; a log sink
		// is not.
		ev := h.log.Info()
		if res.Replayed {
			ev = h.log.Debug()
		}
		ev.Str("provider", string(provider)).
			Str("event_id", logger.MaskID(res.EventID)).
			Str("outcome", string(res.Outcome)).
			Bool("replayed", res.Replayed).
			Msg("webhook processed")

		// 200 for both the first delivery and every replay. A provider that
		// gets anything else keeps retrying, and a retry storm against an
		// already-correct payment is exactly what the dedup exists to make
		// harmless -- so tell it the truth: we have this event.
		httpx.JSON(w, r, http.StatusOK, map[string]any{
			"received": true,
			"replayed": res.Replayed,
			"outcome":  res.Outcome,
		})
	}
}

// respondError chooses a status the provider will interpret correctly.
//
// The distinction that matters: a signature failure must be a hard 401 that the
// provider does not retry, while an unmatched payment must be a 404 that it
// does -- the usual cause is that the webhook overtook appointment.created by a
// few hundred milliseconds, and the retry in thirty seconds will succeed.
func (h *WebhookHandler) respondError(w http.ResponseWriter, r *http.Request, provider ProviderName, err error) {
	switch {
	case errors.Is(err, ErrProviderNotConfigured):
		h.log.Error().Err(err).Str("provider", string(provider)).
			Msg("webhook received for a rail this deployment has no credentials for")
		httpx.Error(w, r, httpx.NewError(http.StatusNotImplemented, httpx.CodeProviderError,
			"this deployment is not configured for that payment provider"))
		return
	case errors.Is(err, ErrWebhookMalformed):
		// Authenticated but unreadable: a 400 tells the provider "your body is
		// wrong", where a 401 would tell them "your credentials are wrong" and
		// send them down the wrong path entirely.
		h.log.Warn().Err(err).Str("provider", string(provider)).
			Msg("webhook authenticated but its body could not be read")
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
			"webhook signature is valid but the body could not be read"))
		return
	case errors.Is(err, ErrSignatureInvalid), errors.Is(err, ErrEventTooOld):
		// Never log the body, never echo the reason back in detail. An
		// attacker probing signature verification learns nothing from us.
		h.log.Warn().
			Str("provider", string(provider)).
			Str("ip", r.Header.Get("X-Forwarded-For")).
			Bool("expired", errors.Is(err, ErrEventTooOld)).
			Msg("rejected webhook with an invalid or stale signature")
		httpx.Error(w, r, httpx.NewError(http.StatusUnauthorized, httpx.CodeSignatureInvalid,
			"signature verification failed"))

	case errors.Is(err, ErrWebhookAmountMismatch):
		// 409, not 200 and not 500. A 200 would make the disagreement
		// invisible on both sides -- the rail would consider the delivery
		// successful and stop, and nobody would ever look. A 5xx would say
		// "we broke, try again", which is untrue and buys a retry storm. 409
		// says: recorded, refused, and not going to change on a retry. The
		// event row is already committed with the discrepancy, so a redelivery
		// deduplicates and answers 200 rather than re-alarming.
		//
		// The reason is deliberately not echoed to the caller. An attacker
		// probing for the amount a payment expects must not be told it.
		h.log.Error().Err(err).Str("provider", string(provider)).
			Msg("webhook reported an amount or currency that disagrees with the payment; refused")
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeWebhookAmountMismatch,
			"the reported amount does not match this payment"))

	case errors.Is(err, ErrNotFound):
		h.log.Warn().Str("provider", string(provider)).
			Msg("webhook could not be matched to a payment; asking the provider to retry")
		httpx.Error(w, r, httpx.NewError(http.StatusNotFound, httpx.CodeNotFound,
			"no payment matches this event yet"))

	default:
		h.log.Error().Err(err).Str("provider", string(provider)).Msg("webhook processing failed")
		httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
	}
}
