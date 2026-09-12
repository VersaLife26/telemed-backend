package user

import (
	"context"
	"errors"
)

// Domain errors. Handlers translate these to httpx.APIError; the service and
// repository layers never import httpx (handler -> service -> repository is
// a one-way dependency, enforced by review).
var (
	ErrInvalidPhone          = errors.New("user: invalid phone number")
	ErrRateLimited           = errors.New("user: rate limit exceeded")
	ErrOTPExpired            = errors.New("user: otp expired or not found")
	ErrOTPInvalid            = errors.New("user: otp does not match")
	ErrOTPLocked             = errors.New("user: too many verify attempts, otp invalidated")
	ErrUserNotFound          = errors.New("user: not found")
	ErrUserSuspended         = errors.New("user: account suspended")
	ErrUserDeleted           = errors.New("user: account deleted")
	ErrRefreshInvalid        = errors.New("user: refresh token invalid")
	ErrRefreshExpired        = errors.New("user: refresh token expired")
	ErrRefreshReused         = errors.New("user: refresh token reuse detected, session revoked")
	ErrFamilyNotFound        = errors.New("user: family member not found")
	ErrForbidden             = errors.New("user: caller may not access this resource")
	ErrEmailTaken            = errors.New("user: email already registered")
	ErrPhoneTaken            = errors.New("user: phone already registered")
	ErrGoogleTaken           = errors.New("user: google account already registered")
	ErrInvalidEmail          = errors.New("user: invalid email")
	ErrInvalidPassword       = errors.New("user: invalid password")
	ErrInvalidCredentials    = errors.New("user: invalid credentials")
	ErrGoogleDisabled        = errors.New("user: google sign-in is not configured")
	ErrGoogleTokenInvalid    = errors.New("user: google id token is invalid")
	ErrGoogleEmailUnverified = errors.New("user: google email is not verified")

	// ErrOTPIdentityRequired and ErrOTPIdentityAmbiguous police the "exactly
	// one of phone or email" rule on the OTP front door.
	ErrOTPIdentityRequired  = errors.New("user: an otp needs a phone number or an email address")
	ErrOTPIdentityAmbiguous = errors.New("user: send an otp to a phone number or an email address, not both")

	// ErrNoDeliveryAddress is the phone-identity case that cannot be served
	// while email is the only transport: the number has no account, or the
	// account has no email on it, so there is nowhere to send the code. It is
	// deliberately a distinct error rather than ErrUserNotFound -- the caller
	// needs to be told to use an email address, not that the account is
	// missing, and a new registration by phone lands here every time.
	ErrNoDeliveryAddress = errors.New("user: no email address on file for this phone number")

	// ErrOTPDeliveryUnavailable means no OTP transport is wired at all.
	ErrOTPDeliveryUnavailable = errors.New("user: no otp delivery transport is configured")
)

// EmailSender delivers one transactional email. It is the OTP transport for
// this deployment: SMS is not available, so login and registration codes go
// out over SMTP for phone and email identities alike.
//
// Business logic depends on this interface rather than net/smtp for the same
// reason SMSProvider exists -- swapping SMTP for a vendor API is one adapter.
type EmailSender interface {
	// Send delivers a single message and returns a provider message id for
	// support correlation, or an error if the message was rejected.
	Send(ctx context.Context, to, subject, body string) (providerMessageID string, err error)
}

// SMSProvider delivers an OTP (or any transactional text) to a phone number.
// Business logic never imports a vendor SDK directly -- it depends on this
// interface, so swapping Dialog for Twilio, or adding a third provider in
// 2040, is a config change plus one adapter.
type SMSProvider interface {
	// Send delivers body to phone (E.164) and returns a provider-assigned
	// message id for support correlation, or an error if the provider
	// rejected or could not reach the send API.
	Send(ctx context.Context, phone, body string) (providerMessageID string, err error)
}

// KeycloakClient mirrors the subset of gocloak the service needs, so the
// domain layer never imports gocloak directly and so tests can fake it.
//
// CreateUser must be safe to call when Keycloak is unreachable: it returns an
// error, the caller logs and continues, and the platform's own JWT issuance
// is entirely independent of Keycloak's availability. See README "50-Year
// Maintenance" for why the OTP login path does not depend on Keycloak at all.
type KeycloakClient interface {
	// CreateUser provisions a mirrored identity in the telemedicine realm,
	// carrying a telemed_user_id attribute so a future SSO or passkey login
	// (see SDD section 12) can be linked back to this user. Returns the
	// Keycloak-assigned user id.
	CreateUser(ctx context.Context, u User) (keycloakID string, err error)
}

// GoogleIdentity is the verified payload of a Google ID token. Only fields
// this service actually uses to find-or-create an account are kept.
type GoogleIdentity struct {
	Sub           string
	Email         string
	EmailVerified bool
	Name          string
}

// GoogleVerifier checks a Google ID token and returns the identity Google
// attested. The domain layer never talks to Google's HTTP APIs directly, so
// tests can inject a stub and production can swap tokeninfo for a JWKS
// verifier without touching Register/Login.
type GoogleVerifier interface {
	Verify(ctx context.Context, idToken string) (GoogleIdentity, error)
}
