package gateway

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// defaultRoutesJSON is the route table shipped inside the binary. It is the
// table that runs when no external ROUTES_FILE is configured, which means
// `go build` alone produces a fully working gateway -- no config directory
// has to travel with the binary for the default eight backend services (see
// the report: the brief's task text says "nine backend services" but the
// brief's own port table lists eight -- api-gateway plus eight, not nine).
//
// This is a byte-for-byte copy of config/routes.json, which ships alongside
// the binary purely as the starter file an operator edits in place: point
// ROUTES_FILE at it (or a copy) to add a service without a rebuild.
//
//go:embed routes.default.json
var defaultRoutesJSON []byte

// AuthMode says how a route treats the caller.
type AuthMode string

const (
	// AuthPublic bypasses JWT verification entirely. Only ever set by an
	// explicit config entry -- never inferred from a path pattern.
	AuthPublic AuthMode = "public"
	// AuthAuthenticated requires a valid bearer token. Roles, if any, further
	// restrict which authenticated principals may proceed.
	AuthAuthenticated AuthMode = "authenticated"
	// AuthAdmin requires a valid bearer token carrying an admin role AND
	// membership of the admin IP allowlist.
	AuthAdmin AuthMode = "admin"
)

// TimeoutClass buckets a route into one of the three latencies the brief
// calls out: a single global timeout is wrong for all of them.
type TimeoutClass string

const (
	TimeoutFast   TimeoutClass = "fast"   // 5s  -- reads
	TimeoutWrite  TimeoutClass = "write"  // 15s -- writes
	TimeoutUpload TimeoutClass = "upload" // 60s -- file/PDF traffic
)

// RateLimitTier names one bucket configuration. The concrete Requests/Window
// pairs live in ratelimit.go so this file stays pure data shape.
type RateLimitTier string

const (
	RateLimitStrictOTP RateLimitTier = "strict_otp"
	// RateLimitStrictOTPVerify is the OTP tier for CODE VERIFICATION, and it
	// is a separate tier from strict_otp rather than a variant of it because
	// the two routes bound different things. strict_otp bounds SMS spend: a
	// send costs money and puts a message on a real subscriber's handset.
	// Verification sends nothing -- it bounds guessing, and its correct budget
	// is a multiple of the send budget, not the same number. Sharing one tier
	// meant sharing one bucket, which spent the SMS allowance on typos.
	RateLimitStrictOTPVerify RateLimitTier = "strict_otp_verify"
	RateLimitModerate        RateLimitTier = "moderate"
	RateLimitGenerous        RateLimitTier = "generous"
	RateLimitAdmin           RateLimitTier = "admin"
	RateLimitWebhook         RateLimitTier = "webhook"
)

// RouteRule is one entry in the gateway's route table. The table itself is
// data (config/routes.json), not Go code: adding a tenth backend service is
// adding an entry here and restarting, not shipping a new binary.
type RouteRule struct {
	Name      string        `json:"name"`
	Pattern   string        `json:"pattern"`
	Methods   []string      `json:"methods"` // empty/absent == every method
	Upstream  string        `json:"upstream"`
	Auth      AuthMode      `json:"auth"`
	Roles     []string      `json:"roles,omitempty"`
	Timeout   TimeoutClass  `json:"timeout_class"`
	RateLimit RateLimitTier `json:"rate_limit"`
	// RateLimitKeyField names a top-level JSON body field (e.g. "phone")
	// that, when present, keys an additional rate-limit bucket alongside the
	// per-IP one. Used by the OTP routes: strict per phone AND per IP.
	RateLimitKeyField string `json:"rate_limit_key_field,omitempty"`
	// BodyLimitBytes overrides the timeout class's default body cap when
	// non-zero.
	BodyLimitBytes int64  `json:"body_limit_bytes,omitempty"`
	Description    string `json:"description,omitempty"`
}

type routeFile struct {
	Routes []RouteRule `json:"routes"`
}

// LoadRoutes reads the route table. path, when non-empty, is tried first so
// an operator can add a new upstream without rebuilding the image; on any
// error reading or parsing that file, or when path is empty, the table
// embedded at build time is used instead.
func LoadRoutes(path string) ([]RouteRule, error) {
	raw := defaultRoutesJSON
	if path != "" {
		// G304 is a false positive here, and the distinction matters because
		// the route table decides which paths are public. path is ROUTES_FILE:
		// a deployment setting read once at boot from the process environment,
		// which is the same trust domain as the binary and the image itself.
		// It is never derived from a request, a header, a database row or an
		// event -- there is no request in scope at boot. An operator able to
		// set ROUTES_FILE can already replace the whole container. Cleaned
		// first so the path in the error message is the one actually opened.
		clean := filepath.Clean(path)
		if b, err := os.ReadFile(clean); err == nil { //nolint:gosec // G304: ROUTES_FILE is boot-time deployment config, not request-derived input; see the comment above.
			raw = b
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("gateway: read routes file %s: %w", clean, err)
		}
	}

	var rf routeFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("gateway: parse route table: %w", err)
	}
	if len(rf.Routes) == 0 {
		return nil, fmt.Errorf("gateway: route table is empty")
	}
	// Indexed rather than ranged by value: RouteRule is 184 bytes and this
	// runs over the whole table.
	for i := range rf.Routes {
		r := &rf.Routes[i]
		if r.Name == "" {
			return nil, fmt.Errorf("gateway: route table entry %d has no name", i)
		}
		if r.Pattern == "" {
			return nil, fmt.Errorf("gateway: route %q has no pattern", r.Name)
		}
		if r.Upstream == "" {
			return nil, fmt.Errorf("gateway: route %q has no upstream", r.Name)
		}
		switch r.Auth {
		case AuthPublic, AuthAuthenticated, AuthAdmin:
		default:
			return nil, fmt.Errorf("gateway: route %q has invalid auth mode %q", r.Name, r.Auth)
		}
		switch r.Timeout {
		case TimeoutFast, TimeoutWrite, TimeoutUpload:
		default:
			return nil, fmt.Errorf("gateway: route %q has invalid timeout_class %q", r.Name, r.Timeout)
		}
		switch r.RateLimit {
		case RateLimitStrictOTP, RateLimitStrictOTPVerify, RateLimitModerate, RateLimitGenerous, RateLimitAdmin, RateLimitWebhook:
		default:
			return nil, fmt.Errorf("gateway: route %q has invalid rate_limit %q", r.Name, r.RateLimit)
		}
	}
	return rf.Routes, nil
}

// PublicRoutes returns the subset of rules that bypass authentication. Tests
// use this to assert the public surface is exactly the documented allowlist,
// no more and no less.
func PublicRoutes(rules []RouteRule) []RouteRule {
	var out []RouteRule
	for i := range rules {
		if rules[i].Auth == AuthPublic {
			out = append(out, rules[i])
		}
	}
	return out
}

// AdminRoutes returns the subset of rules that require the admin auth mode.
func AdminRoutes(rules []RouteRule) []RouteRule {
	var out []RouteRule
	for i := range rules {
		if rules[i].Auth == AuthAdmin {
			out = append(out, rules[i])
		}
	}
	return out
}
