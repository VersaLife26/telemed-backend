package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	platmw "telemed/internal/platform/middleware"
)

// tierSpec is one rate-limit bucket's shape. Concrete numbers live here, in
// one place, so "how strict is strict" is a single readable table rather
// than scattered literals.
type tierSpec struct {
	Requests int
	Window   time.Duration
}

// tiers is deliberately conservative on the OTP path (the platform's own
// abuse surface: SMS costs money and a phone number is a scarce, PDPA-
// sensitive identifier) and generous on ordinary authenticated traffic.
var tiers = map[RateLimitTier]tierSpec{
	RateLimitModerate: {Requests: 60, Window: time.Minute},
	RateLimitGenerous: {Requests: 300, Window: time.Minute},
	RateLimitAdmin:    {Requests: 120, Window: time.Minute},
	RateLimitWebhook:  {Requests: 120, Window: time.Minute},
}

// The OTP tiers are dual-keyed: a per-IP bucket and a per-phone bucket, both
// of which must admit the request.
//
// SEND and VERIFY get SEPARATE buckets, which they did not always. Both routes
// carried one tier and buildRateLimiters derived the key from the tier alone,
// so they incremented the same counters. The per-phone allowance is 3 an hour
// and exists to bound SMS spend; sharing it with verification spent it on
// requests that send no message. A successful login costs 2 of the 3, one
// mistyped digit costs the last, and the user is locked out of the only login
// path on the platform for an hour -- with user-service's own "5 attempts per
// code" policy never reached, because the gateway answered first.
//
// The docs' OTP flow describes "if >3 return 429" per hour per phone. That is
// the SEND rule, and it is kept exactly. The per-IP caps are looser and exist
// so one address cannot script an SMS-bombing run across many numbers.
var (
	otpIPSpec        = tierSpec{Requests: 20, Window: time.Hour}
	otpPhoneSendSpec = tierSpec{Requests: 3, Window: time.Hour}

	// otpPhoneVerifySpec is DERIVED, not chosen: user-service issues at most
	// otpPhoneSendSpec.Requests codes per phone per hour and allows
	// otpVerifyAttemptsPerCode guesses against each before destroying it
	// (internal/user/service.go). The gateway must admit their product or it
	// silently overrides a policy it does not own. Guessing is still bounded
	// hard -- 15 attempts an hour against a 6-digit code is 15 in 10^6, and
	// the code dies after 5 wrong guesses long before the budget matters.
	otpPhoneVerifySpec = tierSpec{Requests: otpPhoneSendSpec.Requests * otpVerifyAttemptsPerCode, Window: time.Hour}
)

// otpVerifyAttemptsPerCode mirrors otpVerifyMaxAttempts in user-service. It is
// restated rather than imported because the gateway shares no code with a
// backend service by design; the test that derives otpPhoneVerifySpec from it
// is what keeps the two from drifting silently.
const otpVerifyAttemptsPerCode = 5

// maxOTPBodyPeek bounds how much of an OTP request body the gateway buffers
// to extract the phone number for keying. The route's real body size cap is
// enforced separately (and is larger); this only limits the peek.
const maxOTPBodyPeek = 4 << 10

// buildRateLimiters returns the ordered middleware chain for a route's rate
// limit tier. The OTP tiers yield two limiters (IP, then phone); every other
// tier yields exactly one.
//
// Every bucket name is derived from the TIER, which is what makes two routes
// on the same tier share a budget. That is correct for moderate/generous --
// they are a general traffic allowance -- and it was wrong for the OTP pair,
// which is why send and verify are now distinct tiers rather than distinct
// routes on one tier.
func buildRateLimiters(tier RateLimitTier, keyField string, c cache.Cache, log zerolog.Logger) []func(http.Handler) http.Handler {
	switch tier {
	case RateLimitStrictOTP, RateLimitStrictOTPVerify:
		ipName, phoneName, phoneSpec := "otp_send_ip", "otp_send_phone", otpPhoneSendSpec
		if tier == RateLimitStrictOTPVerify {
			ipName, phoneName, phoneSpec = "otp_verify_ip", "otp_verify_phone", otpPhoneVerifySpec
		}
		mws := []func(http.Handler) http.Handler{
			platmw.RateLimit(c, platmw.RateLimitConfig{
				Name: ipName, Requests: otpIPSpec.Requests, Window: otpIPSpec.Window,
			}, log),
		}
		if keyField != "" {
			mws = append(mws, phoneRateLimit(c, phoneName, keyField, phoneSpec, log))
		}
		return mws
	default:
		spec, ok := tiers[tier]
		if !ok {
			spec = tiers[RateLimitGenerous]
		}
		return []func(http.Handler) http.Handler{
			platmw.RateLimit(c, platmw.RateLimitConfig{
				Name: string(tier), Requests: spec.Requests, Window: spec.Window,
				KeyFunc: platmw.ByPrincipal,
			}, log),
		}
	}
}

// otpPhoneBucket derives the per-phone rate-limit bucket key, fixing two
// defects that shipped together in the raw `"phone:" + v` this replaces.
//
// 1. NOT NORMALIZING was a bypass. The budget is meant to be per phone number;
// keying on the string the client typed made "+94771234567", "94771234567",
// "0771234567" and "077-123 4567" four independent 3-per-hour buckets for one
// keypad. user-service normalizes with the same function before applying its
// own durable per-phone limit (internal/user/service.go:77), so the two
// counters were also counting different things -- the gateway's was simply
// looser, and the looser of two limits is the limit.
//
// 2. THE RAW NUMBER WAS THE REDIS KEY. Every OTP request wrote a key literally
// containing a subscriber's phone number, visible to SCAN, MONITOR, and any
// RDB/AOF snapshot or backup. Under the PDPA a phone number is personal data;
// a rate limiter has no business being the place it is retained in the clear.
// SHA-256 keeps the bucket exactly as discriminating -- equal numbers still
// collide, unequal ones still do not -- while making the key itself carry
// nothing.
//
// An unparseable value still spends budget (a malformed request must not be a
// way to skip the limiter) but is scoped by caller, so one bad value cannot
// share a bucket with every other bad value on the platform.
func otpPhoneBucket(raw string, r *http.Request) string {
	norm := httpx.NormalizePhone(raw)
	if norm == "" {
		return "phone-invalid:" + platmw.ByPrincipal(r)
	}
	sum := sha256.Sum256([]byte(norm))
	return "phone:" + hex.EncodeToString(sum[:])
}

// phoneRateLimit enforces spec keyed on the normalized phone number found in
// a top-level JSON body field named keyField. It reads up to
// maxOTPBodyPeek bytes, restores the body byte-for-byte for every later
// reader (validation downstream, and eventually the proxied upstream call),
// and falls back to an IP-scoped "unknown" bucket when the field is missing
// or the body is not JSON -- a malformed request still spends rate-limit
// budget rather than skipping the limiter outright.
func phoneRateLimit(c cache.Cache, bucket, keyField string, spec tierSpec, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "unknown:" + platmw.ByPrincipal(r)

			if r.Body != nil {
				peeked, err := io.ReadAll(io.LimitReader(r.Body, maxOTPBodyPeek+1))
				_ = r.Body.Close()
				if err == nil {
					// Restore the body exactly as received, regardless of
					// whether we found a usable phone field in it.
					r.Body = io.NopCloser(bytes.NewReader(peeked))
					r.ContentLength = int64(len(peeked))

					if len(peeked) <= maxOTPBodyPeek {
						var doc map[string]any
						if json.Unmarshal(peeked, &doc) == nil {
							if v, ok := doc[keyField].(string); ok && v != "" {
								key = otpPhoneBucket(v, r)
							}
						}
					}
				}
			}

			n, err := c.Incr(r.Context(), "ratelimit:"+bucket+":"+key, spec.Window)
			if err != nil {
				log.Error().Err(err).Msg("otp phone rate limiter unavailable, failing open")
				next.ServeHTTP(w, r)
				return
			}

			remaining := spec.Requests - int(n)
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(spec.Requests))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(spec.Window).Unix(), 10))

			if int(n) > spec.Requests {
				w.Header().Set("Retry-After", strconv.Itoa(int(spec.Window.Seconds())))
				httpx.Error(w, r, httpx.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
