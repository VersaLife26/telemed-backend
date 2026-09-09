// Package httpx holds the HTTP request/response conventions shared by every
// telemed service: one JSON envelope, one error taxonomy, one set of helpers.
//
// A single error shape across 10 services is what lets the TypeScript Next.js
// clients write error handling once instead of ten times.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-playground/validator/v10"

	"telemed/internal/platform/logger"
)

// ErrorCode is a stable, machine-readable error identifier. Clients switch on
// these; the human-readable message may be reworded or translated at any time,
// the code may not.
type ErrorCode string

const (
	CodeBadRequest       ErrorCode = "BAD_REQUEST"
	CodeValidation       ErrorCode = "VALIDATION_FAILED"
	CodeUnauthorized     ErrorCode = "UNAUTHORIZED"
	CodeForbidden        ErrorCode = "FORBIDDEN"
	CodeNotFound         ErrorCode = "NOT_FOUND"
	CodeConflict         ErrorCode = "CONFLICT"
	CodeSlotUnavailable  ErrorCode = "SLOT_UNAVAILABLE"
	CodeSlotLocked       ErrorCode = "SLOT_LOCKED"
	CodeRateLimited      ErrorCode = "RATE_LIMITED"
	CodePaymentRequired  ErrorCode = "PAYMENT_REQUIRED"
	CodeUnprocessable    ErrorCode = "UNPROCESSABLE"
	CodePaymentNotReady  ErrorCode = "PAYMENT_NOT_READY" // 409, retryable: appointment.created still in flight
	CodeSignatureInvalid ErrorCode = "SIGNATURE_INVALID" // 401, webhook signature verification failed
	// CodeAccountSuspended is 403 and terminal for the caller: unlike
	// FORBIDDEN, which says "not for you", it says "this account is stopped".
	// A client must show it rather than retrying or silently re-authenticating
	// -- a suspended user who is bounced to the login screen will simply log
	// in again, see it fail, and file a support ticket nobody can action.
	CodeAccountSuspended ErrorCode = "ACCOUNT_SUSPENDED"
	CodeRefundNotAllowed ErrorCode = "REFUND_NOT_ALLOWED"
	CodeProviderError    ErrorCode = "PROVIDER_ERROR" // 502 unavailable / 501 unsupported
	// CodeNoteFinalised is distinct from CodeConflict because the two demand
	// opposite client behaviour: CONFLICT means re-read and retry;
	// NOTE_FINALISED means retrying can never work -- amend instead. A client
	// that conflates them retries forever.
	CodeNoteFinalised ErrorCode = "NOTE_FINALISED"
	// CodeHolidayHasBookings refuses leave that would cancel live
	// appointments. It carries fields.booked_appointments so the doctor can
	// see what they are about to cancel and re-send with explicit consent.
	CodeHolidayHasBookings ErrorCode = "HOLIDAY_HAS_BOOKINGS"
	// CodeWebhookAmountMismatch is 409: a signed, authentic webhook reported an
	// amount or currency that disagrees with the stored payment. That is either
	// a provider bug or an attack, and both deserve a loud, non-retryable
	// answer rather than silent acceptance.
	CodeWebhookAmountMismatch ErrorCode = "WEBHOOK_AMOUNT_MISMATCH"
	CodeInternal              ErrorCode = "INTERNAL_ERROR"
	CodeUnavailable           ErrorCode = "SERVICE_UNAVAILABLE"
	CodeTimeout               ErrorCode = "TIMEOUT"

	// --- promotions and carrier billing ---------------------------------
	//
	// Added by telemed-payment-service for the promo-code and Dialog PIN
	// surfaces the mobile apps call. They are distinct codes rather than a
	// shared UNPROCESSABLE because the patient-facing copy is different for
	// every one of them: "we don't know that code", "that code has expired",
	// "that code is all used up" and "that code doesn't apply to this
	// booking" are four different things to say, and a client that can only
	// see UNPROCESSABLE has to say the vaguest of them.
	CodePromoInvalid        ErrorCode = "PROMO_INVALID"          // 422, unknown or deactivated code
	CodePromoExpired        ErrorCode = "PROMO_EXPIRED"          // 422, outside the validity window
	CodePromoExhausted      ErrorCode = "PROMO_EXHAUSTED"        // 409, code or per-user budget used up
	CodePromoNotApplicable  ErrorCode = "PROMO_NOT_APPLICABLE"   // 422, wrong currency, under the minimum, or the price is locked
	CodePINNotOutstanding   ErrorCode = "PIN_NOT_OUTSTANDING"    // 409, no challenge is waiting to be answered
	CodePINInvalid          ErrorCode = "PIN_INVALID"            // 422, wrong PIN, attempts remain
	CodePINExpired          ErrorCode = "PIN_EXPIRED"            // 409, the carrier's window closed; request a new PIN
	CodePINAttemptsExceeded ErrorCode = "PIN_ATTEMPTS_EXCEEDED"  // 409, allowance used up and the payment failed
	CodePaymentMethodError  ErrorCode = "PAYMENT_METHOD_INVALID" // 422, the card setup did not complete
)

// APIError is the single error envelope returned by every endpoint.
type APIError struct {
	Code      ErrorCode         `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`

	status int   `json:"-"`
	cause  error `json:"-"`
}

func (e *APIError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *APIError) Unwrap() error { return e.cause }

// Status returns the HTTP status this error maps to.
func (e *APIError) Status() int {
	if e.status == 0 {
		return http.StatusInternalServerError
	}
	return e.status
}

// WithCause attaches the underlying error for logging. The cause is never
// serialized to the client, which is how internal detail stays internal.
func (e *APIError) WithCause(err error) *APIError {
	clone := *e
	clone.cause = err
	return &clone
}

// NewError builds an APIError.
func NewError(status int, code ErrorCode, msg string) *APIError {
	return &APIError{Code: code, Message: msg, status: status}
}

// Prebuilt errors for the cases every service hits.
var (
	ErrBadRequest   = NewError(http.StatusBadRequest, CodeBadRequest, "request could not be parsed")
	ErrUnauthorized = NewError(http.StatusUnauthorized, CodeUnauthorized, "authentication required")
	ErrForbidden    = NewError(http.StatusForbidden, CodeForbidden, "insufficient permissions")
	// ErrAccountSuspended is returned to a caller whose account an
	// administrator has suspended, for as long as any access token issued
	// before the suspension could still be within its lifetime.
	ErrAccountSuspended = NewError(http.StatusForbidden, CodeAccountSuspended, "this account is suspended")
	ErrNotFound         = NewError(http.StatusNotFound, CodeNotFound, "resource not found")
	ErrConflict         = NewError(http.StatusConflict, CodeConflict, "resource state conflict")
	ErrRateLimited      = NewError(http.StatusTooManyRequests, CodeRateLimited, "rate limit exceeded")
	ErrInternal         = NewError(http.StatusInternalServerError, CodeInternal, "internal server error")
	ErrUnavailable      = NewError(http.StatusServiceUnavailable, CodeUnavailable, "service temporarily unavailable")
)

// Meta carries pagination for list endpoints.
type Meta struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// Envelope wraps every successful response so clients parse one shape.
type Envelope struct {
	Data any   `json:"data"`
	Meta *Meta `json:"meta,omitempty"`
}

// JSON writes a JSON body with the given status.
func JSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil || status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already on the wire, so the only useful action
		// left is to record it for the operator.
		_ = err
	}
}

// OK writes 200 with the standard envelope.
func OK(w http.ResponseWriter, r *http.Request, data any) {
	JSON(w, r, http.StatusOK, Envelope{Data: data})
}

// Created writes 201 with the standard envelope.
func Created(w http.ResponseWriter, r *http.Request, data any) {
	JSON(w, r, http.StatusCreated, Envelope{Data: data})
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter, r *http.Request) {
	JSON(w, r, http.StatusNoContent, nil)
}

// List writes 200 with pagination metadata.
func List(w http.ResponseWriter, r *http.Request, data any, meta Meta) {
	if meta.PerPage > 0 {
		meta.TotalPages = int((meta.Total + int64(meta.PerPage) - 1) / int64(meta.PerPage))
	}
	JSON(w, r, http.StatusOK, Envelope{Data: data, Meta: &meta})
}

// Error writes err as the standard error envelope, and logs the cause.
//
// The client gets a generic 500 with no detail: an error string can embed a
// patient name, a column value from a constraint violation, or an SQL
// fragment, and none of that belongs on the wire.
//
// But the cause MUST reach the operator, and for a long time it did not. This
// function captured it into the APIError and then dropped it on the floor, so
// every 500 on the platform arrived as `{"code":"INTERNAL_ERROR"}` with the
// actual failure existing nowhere at all. A single pgx encode error cost an
// hour to find for exactly that reason. Logging the cause here is what makes a
// 500 diagnosable, and it happens in one place so no handler can forget.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := &APIError{}
	if !errors.As(err, &apiErr) {
		apiErr = ErrInternal.WithCause(err)
	}
	out := *apiErr
	out.RequestID = middleware.GetReqID(r.Context())

	logCause(r, &out)
	JSON(w, r, out.Status(), out)
}

// logCause records the underlying error server-side. 5xx is an operator's
// problem and logs at error; 4xx is the caller's and logs at debug, so a client
// looping on a bad request cannot flood the log.
func logCause(r *http.Request, e *APIError) {
	cause := e.cause
	if cause == nil && e.Status() < 500 {
		// A 4xx with no cause is fully described by the envelope already.
		return
	}

	log := logger.FromContext(r.Context())
	ev := log.Debug()
	if e.Status() >= 500 {
		ev = log.Error()
	}

	ev = ev.Str("code", string(e.Code)).
		Int("status", e.Status()).
		Str("method", r.Method).
		Str("route", routePattern(r))

	if cause != nil {
		ev = ev.Err(cause)
	}
	if len(e.Fields) > 0 {
		// Field NAMES are safe and useful; the rejected values are not, and
		// ValidationError never puts them here.
		names := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			names = append(names, k)
		}
		sort.Strings(names)
		ev = ev.Strs("invalid_fields", names)
	}
	ev.Msg("request failed")
}

// routePattern prefers chi's matched pattern over the raw path, so identifiers
// stay out of the log and the lines aggregate. An unmatched request has no
// pattern, and the raw path it used to fall back to is the one case where the
// path is most likely to carry an identifier -- so the fallback is masked.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
		return rctx.RoutePattern()
	}
	return logger.SafePath(r.URL.Path)
}

// ValidationError converts validator.ValidationErrors into a 422 with a
// field->reason map the mobile clients render inline.
func ValidationError(err error) *APIError {
	fields := map[string]string{}
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		for _, fe := range ve {
			fields[jsonFieldName(fe)] = describeTag(fe)
		}
	}
	return &APIError{
		Code:    CodeValidation,
		Message: "one or more fields failed validation",
		Fields:  fields,
		status:  http.StatusUnprocessableEntity,
		cause:   err,
	}
}

func jsonFieldName(fe validator.FieldError) string {
	// validator reports the Go field name; the clients only know the JSON name.
	// Namespace() gives "Struct.Field", so take the trailing segment and
	// lowercase it as a reasonable default for our snake_case JSON tags.
	name := fe.Field()
	return toSnake(name)
}

// toSnake converts a Go field name to the snake_case JSON name, treating runs
// of capitals as single acronyms.
//
// The naive "underscore before every capital" version turns SLMCNumber into
// s_l_m_c_number and DoctorID into doctor_i_d. Those names appear in the
// "fields" map of a 422 response, so a mobile client trying to highlight the
// offending input looks for a key that exists nowhere in the API and silently
// highlights nothing.
//
// The rule: insert an underscore before a capital only when the previous rune
// was lower-case or a digit, or when the capital is the last of a run and is
// followed by a lower-case letter (the boundary in "SLMCNumber" between C and N).
func toSnake(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	out := make([]rune, 0, len(runes)+4)

	for i, r := range runes {
		if isUpper(r) && i > 0 {
			prev := runes[i-1]
			nextIsLower := i+1 < len(runes) && isLower(runes[i+1])
			if isLower(prev) || isDigit(prev) || (isUpper(prev) && nextIsLower) {
				out = append(out, '_')
			}
		}
		out = append(out, toLower(r))
	}
	return string(out)
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func toLower(r rune) rune {
	if isUpper(r) {
		return r + ('a' - 'A')
	}
	return r
}

func describeTag(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "this field is required"
	case "email":
		return "must be a valid email address"
	case "e164", "sriphone":
		return "must be a valid Sri Lankan mobile number in +947XXXXXXXX form"
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "min":
		return "must be at least " + fe.Param()
	case "max":
		return "must be at most " + fe.Param()
	case "gte":
		return "must be greater than or equal to " + fe.Param()
	case "lte":
		return "must be less than or equal to " + fe.Param()
	case "oneof":
		return "must be one of: " + fe.Param()
	case "len":
		return "must be exactly " + fe.Param() + " characters"
	default:
		return "failed the " + fe.Tag() + " rule"
	}
}
