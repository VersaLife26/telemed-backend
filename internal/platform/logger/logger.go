// Package logger provides structured JSON logging via zerolog with a hard
// guarantee that protected health information never reaches a log sink.
//
// PHI safety is enforced two ways:
//  1. Redact() scrubs known-sensitive keys before they are attached to an event.
//  2. Phone/NIC helpers emit masked forms only.
//
// Compliance requires that logs be useful for debugging while never becoming a
// secondary, unaudited copy of the medical record.
package logger

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// SafePath renders a URL path for a log line with every identifier-shaped
// segment replaced by a placeholder.
//
// It exists because the "use the chi route pattern, never the raw path" rule
// has a hole: chi fills the pattern in during routing, so a request that
// matched no route has no pattern at all, and every call site fell back to
// r.URL.Path. On this platform an unmatched path is routinely
// /api/v1/records/<document-uuid> or
// /api/v1/verify/prescriptions/<prescription-uuid> -- a patient-linkable
// identifier, arriving from a stale mobile client or a mistyped URL, written
// straight into the log stream by the code whose comment promises the
// opposite.
//
// The shape of the path is kept, because an operator debugging a 404 needs to
// know which endpoint a client thought it was calling. Only the values go.
func SafePath(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if identifierish(seg) {
			segments[i] = "-"
		}
	}
	return strings.Join(segments, "/")
}

// identifierish reports whether a path segment looks like a value rather than
// a name. It is deliberately generous: a false positive costs an operator one
// masked segment, a false negative puts a patient identifier in a log sink.
func identifierish(seg string) bool {
	if len(seg) >= 16 {
		return true
	}
	if len(seg) >= 5 {
		digits := true
		for _, r := range seg {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// sensitiveKeys are never logged in full, regardless of caller intent.
var sensitiveKeys = map[string]struct{}{
	"password": {}, "otp": {}, "otp_hash": {}, "token": {}, "access_token": {},
	"refresh_token": {}, "authorization": {}, "cookie": {}, "set-cookie": {},
	"nic": {}, "nic_hash": {}, "bank_account": {}, "bank_encrypted": {},
	"card": {}, "card_number": {}, "cvv": {}, "client_secret": {},
	"symptoms": {}, "diagnosis": {}, "notes": {}, "clinical_notes": {},
	// The four SOAP section names. "notes"/"clinical_notes" only catch a
	// field literally called that; a structured log carrying the note's
	// sections would have named them "subjective", "objective",
	// "assessment", "plan" and walked straight past the denylist.
	// "assessment" and "plan" are ordinary English words, which is exactly
	// why they are easy to attach to a log line without thinking.
	"subjective": {}, "objective": {}, "assessment": {}, "plan": {},
	"soap": {}, "clinical_note": {}, "diagnoses": {}, "chief_complaint": {},
	"intake": {}, "prescription": {}, "drugs": {}, "allergies": {},
	"medical_history": {}, "stripe_secret": {}, "webhook_secret": {},
	"api_key": {}, "secret": {}, "private_key": {}, "signature": {},
}

// New builds a logger. In dev it writes human-readable console output; in every
// other environment it writes single-line JSON for ingestion by Loki/CloudWatch.
func New(serviceName, level, env string) zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.ErrorStackFieldName = "stack"

	lvl, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil || level == "" {
		lvl = zerolog.InfoLevel
	}

	var w io.Writer = os.Stdout
	if env == "dev" || env == "development" {
		w = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}

	return zerolog.New(w).
		Level(lvl).
		With().
		Timestamp().
		Str("service", serviceName).
		Str("env", env).
		Logger()
}

// Redact returns a copy of fields with sensitive values replaced. Use it before
// logging any map that originated from a request body or an external webhook.
func Redact(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if _, bad := sensitiveKeys[strings.ToLower(k)]; bad {
			out[k] = "[REDACTED]"
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = Redact(nested)
			continue
		}
		out[k] = v
	}
	return out
}

// MaskPhone keeps only the last four digits of a phone number. Enough to
// correlate a support ticket, not enough to identify a patient from logs alone.
func MaskPhone(phone string) string {
	if len(phone) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}

// MaskID keeps the first 8 characters of a UUID. Collisions are irrelevant for
// log correlation and the full identifier stays out of the log stream.
func MaskID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "..."
}

// WithContext stores the logger on the context so handlers deep in the stack
// can log with the request-scoped fields already attached.
func WithContext(ctx context.Context, l zerolog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext retrieves the request-scoped logger, falling back to a disabled
// logger so a missing logger can never panic a request.
func FromContext(ctx context.Context) zerolog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(zerolog.Logger); ok {
		return l
	}
	return zerolog.Nop()
}
