package user

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
)

// Tunables for the OTP front door. These are constants rather than config
// because they are security parameters, not deployment knobs -- changing
// them is a code review, not an environment variable.
const (
	otpTTL               = 5 * time.Minute
	otpSendLimitPerPhone = 3
	otpSendLimitWindow   = time.Hour
	otpVerifyMaxAttempts = 5

	emailAuthLimitPerEmail = 10
	emailAuthLimitWindow   = time.Hour

	// erasureGracePeriod is how long a soft-deleted account's PII survives
	// before the reaper anonymises it. It gives support a window to reverse
	// an accidental or coerced deletion request.
	erasureGracePeriod = 30 * 24 * time.Hour
)

// Service implements every business rule for identity, OTP auth, sessions,
// and family profiles. It has no knowledge of HTTP or SQL syntax.
type Service struct {
	repo    *Repository
	cache   cache.Cache
	limiter *otpLimiter
	outbox  *events.Outbox
	sms     SMSProvider
	// email is the OTP transport. sms is retained and still wired, but the
	// OTP path does not use it while this deployment has no SMS rail -- see
	// deliverOTP/otpAddress. Re-enabling SMS is a branch in otpAddress, not a
	// rewiring.
	email    EmailSender
	keycloak KeycloakClient
	tokens   *TokenIssuer
	google   GoogleVerifier
	// nic keys the NIC digest with a secret the database does not hold. It is
	// a constructor argument rather than an optional setter so that a service
	// wired without one fails to compile, not to protect an identity.
	nic *NICHasher
	log zerolog.Logger

	// doctors is optional. When set, VerifyOTP promotes/creates doctor-role
	// users for phones with an approved public application, then attaches the
	// doctor profile.
	doctors DoctorApplications
}

// NewService wires the domain service. keycloak may be a degraded/no-op
// implementation when Keycloak was unreachable at boot -- see
// cmd/server/main.go and DegradedKeycloakClient.
func NewService(repo *Repository, c cache.Cache, outbox *events.Outbox, sms SMSProvider, kc KeycloakClient, tokens *TokenIssuer, nic *NICHasher, log zerolog.Logger) *Service {
	return &Service{repo: repo, cache: c, limiter: newOTPLimiter(c), outbox: outbox, sms: sms, keycloak: kc, tokens: tokens, nic: nic, log: log}
}

// SetEmailSender attaches the transport every OTP goes out over.
//
// A setter rather than a NewService argument to match SetGoogle and
// SetDoctorApplications, and so the many existing call sites that build a
// Service for tests keep compiling. Without one, SendOTP returns
// ErrOTPDeliveryUnavailable rather than panicking on a nil interface.
func (s *Service) SetEmailSender(e EmailSender) {
	s.email = e
}

// SetDoctorApplications attaches the doctor-service client used to promote
// approved applications (OTP, email login, and the approval consumer).
// Nil leaves those paths as patient-only.
func (s *Service) SetDoctorApplications(d DoctorApplications) {
	s.doctors = d
}

// SetGoogle attaches the Google ID-token verifier. When v is nil, Google
// sign-in returns ErrGoogleDisabled rather than failing boot: a missing
// GOOGLE_CLIENT_ID must not take down phone OTP.
func (s *Service) SetGoogle(v GoogleVerifier) {
	s.google = v
}

// ------------------------------------------------------------------- OTP --

// SendOTPResult is what the handler returns to the client on a successful
// send.
type SendOTPResult struct {
	RequestID         string
	ExpiresIn         time.Duration
	AttemptsRemaining int
}

// OTPIdentity is who a one-time code is for: exactly one of a phone number or
// an email address.
//
// Both are accepted because this deployment has no SMS rail and delivers every
// code over SMTP (see deliverOTP). An email identity addresses itself; a phone
// identity is resolved to the address on that account, and fails closed with
// ErrNoDeliveryAddress when there is none rather than falling back to a
// transport that would print the code to stdout.
type OTPIdentity struct {
	Phone string // E.164, normalised
	Email string // lowercased and trimmed
}

// NewOTPIdentity normalises caller input and enforces exactly one of the two.
func NewOTPIdentity(rawPhone, rawEmail string) (OTPIdentity, error) {
	phone := httpx.NormalizePhone(rawPhone)
	email := NormalizeEmail(rawEmail)

	gavePhone := strings.TrimSpace(rawPhone) != ""
	gaveEmail := strings.TrimSpace(rawEmail) != ""

	switch {
	case gavePhone && gaveEmail:
		return OTPIdentity{}, ErrOTPIdentityAmbiguous
	case gaveEmail:
		if email == "" || !strings.Contains(email, "@") {
			return OTPIdentity{}, ErrInvalidEmail
		}
		return OTPIdentity{Email: email}, nil
	case gavePhone:
		if phone == "" {
			return OTPIdentity{}, ErrInvalidPhone
		}
		return OTPIdentity{Phone: phone}, nil
	default:
		return OTPIdentity{}, ErrOTPIdentityRequired
	}
}

// key is the cache, rate-limit and audit key for this identity. An E.164 phone
// always starts "+" and an email always contains "@", so the two namespaces
// cannot collide and a code sent to one never satisfies a verify on the other.
func (i OTPIdentity) key() string {
	if i.Email != "" {
		return i.Email
	}
	return i.Phone
}

func otpCacheKey(key string) string         { return "otp:code:" + key }
func otpAttemptsCacheKey(key string) string { return "otp:verify_attempts:" + key }
func otpSendCacheKey(key string) string     { return "otp:send:" + key }

// SendOTP generates and delivers a 6-digit code, enforcing the 3-per-hour
// per-phone limit. A separate per-IP limit is applied by
// middleware.RateLimit on the route (see handler.go Routes) using the same
// Cache.Incr primitive, so both limits are fixed-window and shared across
// every replica.
func (s *Service) SendOTP(ctx context.Context, ident OTPIdentity, purpose OTPPurpose, language Language, ip string) (SendOTPResult, error) {
	key := ident.key()
	if key == "" {
		return SendOTPResult{}, ErrOTPIdentityRequired
	}

	// Resolved before the code is generated or the rate limit is spent: a
	// caller who cannot be delivered to should be told so on the first
	// attempt, not burn one of three hourly sends on a message that was
	// never going to arrive.
	to, err := s.otpAddress(ctx, ident)
	if err != nil {
		return SendOTPResult{}, err
	}

	n, allowed, err := s.limiter.allowSend(ctx, key)
	if err != nil {
		return SendOTPResult{}, fmt.Errorf("user: otp send rate limit: %w", err)
	}
	if !allowed {
		s.audit(ctx, key, purpose, ActionSend, ip, false)
		return SendOTPResult{}, ErrRateLimited
	}

	code, err := GenerateOTP()
	if err != nil {
		return SendOTPResult{}, err
	}
	hash, err := HashOTP(code)
	if err != nil {
		return SendOTPResult{}, err
	}
	if err := s.cache.Set(ctx, otpCacheKey(key), []byte(hash), otpTTL); err != nil {
		return SendOTPResult{}, fmt.Errorf("user: store otp: %w", err)
	}
	// A fresh code resets the verify-attempt budget; without this, three
	// wrong guesses against an old code would lock out a code the user
	// never even tried yet.
	_ = s.cache.Del(ctx, otpAttemptsCacheKey(key))

	s.audit(ctx, key, purpose, ActionSend, ip, true)

	if _, err := s.email.Send(ctx, to, otpSubject(purpose, language), otpEmailBody(purpose, language, code)); err != nil {
		s.log.Error().Err(err).Str("to", logger.MaskEmail(to)).Msg("email provider failed to send otp")
		return SendOTPResult{}, fmt.Errorf("user: send otp email: %w", err)
	}

	return SendOTPResult{
		RequestID:         "req_" + uuid.NewString(),
		ExpiresIn:         otpTTL,
		AttemptsRemaining: max(0, otpSendLimitPerPhone-int(n)),
	}, nil
}

// otpAddress decides where this identity's code is delivered.
//
// An email identity is its own address. A phone identity has to be resolved
// against the account, which is the case that fails while SMS is unavailable:
// a number with no account (every new registration) or an account with a null
// email has nowhere to receive a code. That returns ErrNoDeliveryAddress so
// the handler can tell the caller to use an email address, rather than
// reporting a send that silently went nowhere.
func (s *Service) otpAddress(ctx context.Context, ident OTPIdentity) (string, error) {
	if s.email == nil {
		return "", ErrOTPDeliveryUnavailable
	}
	if ident.Email != "" {
		return ident.Email, nil
	}

	u, err := s.repo.FindUserByPhone(ctx, s.repo.Pool(), ident.Phone)
	switch {
	case errors.Is(err, ErrUserNotFound):
		return "", ErrNoDeliveryAddress
	case err != nil:
		return "", fmt.Errorf("user: resolve otp address: %w", err)
	case u.Email == nil || strings.TrimSpace(*u.Email) == "":
		return "", ErrNoDeliveryAddress
	}
	return *u.Email, nil
}

func otpSubject(purpose OTPPurpose, lang Language) string {
	verb := "registration"
	if purpose == PurposeLogin {
		verb = "login"
	}
	switch lang {
	case LanguageSinhala:
		return fmt.Sprintf("VersaLife %s කේතය", verb)
	case LanguageTamil:
		return fmt.Sprintf("VersaLife %s குறியீடு", verb)
	default:
		return fmt.Sprintf("Your VersaLife %s code", verb)
	}
}

// otpEmailBody is the same three-language copy smsBody carries, as plain text.
//
// Plain text, not HTML: the whole message is six digits and a validity window,
// there is nothing to lay out, and a text/plain body cannot carry a tracking
// pixel or a link for a phishing filter to quarantine -- which matters for the
// one message a patient must receive to get in at all.
func otpEmailBody(purpose OTPPurpose, lang Language, code string) string {
	return smsBody(purpose, lang, code)
}

func smsBody(purpose OTPPurpose, lang Language, code string) string {
	// Sri Lanka's three official languages. A production deployment would
	// pull these from a template store; three literal strings is the honest
	// scope for this pass rather than standing up an i18n system unasked.
	verb := "registration"
	if purpose == PurposeLogin {
		verb = "login"
	}
	switch lang {
	case LanguageSinhala:
		return fmt.Sprintf("ඔබගේ %s කේතය %s. විනාඩි 5ක් වලංගුයි.", verb, code)
	case LanguageTamil:
		return fmt.Sprintf("உங்கள் %s குறியீடு %s. 5 நிமிடங்கள் செல்லுபடியாகும்.", verb, code)
	default:
		return fmt.Sprintf("Your %s code is %s. Valid for 5 minutes.", verb, code)
	}
}

// AuthResult is the token pair and profile returned on a successful verify
// or refresh.
type AuthResult struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             User
}

// VerifyOTP checks a code, finds-or-creates the user, and issues a fresh
// session. Verify attempts are capped at 5 per code; the 6th attempt
// invalidates the code outright rather than merely rejecting it, so an
// attacker cannot keep guessing against a code that already failed 5 times
// by simply not triggering whatever counts as a "final" attempt.
func (s *Service) VerifyOTP(ctx context.Context, ident OTPIdentity, code, deviceID string, purpose OTPPurpose, ip string) (AuthResult, error) {
	key := ident.key()
	if key == "" {
		return AuthResult{}, ErrOTPIdentityRequired
	}
	phone := ident.Phone

	_, locked, err := s.limiter.registerVerifyAttempt(ctx, key)
	if err != nil {
		return AuthResult{}, fmt.Errorf("user: otp verify attempts: %w", err)
	}
	if locked {
		_ = s.cache.Del(ctx, otpCacheKey(key))
		s.audit(ctx, key, purpose, ActionVerify, ip, false)
		return AuthResult{}, ErrOTPLocked
	}

	hashBytes, err := s.cache.Get(ctx, otpCacheKey(key))
	if errors.Is(err, cache.ErrNotFound) {
		s.audit(ctx, key, purpose, ActionVerify, ip, false)
		return AuthResult{}, ErrOTPExpired
	}
	if err != nil {
		return AuthResult{}, fmt.Errorf("user: read otp: %w", err)
	}

	if !VerifyOTPHash(string(hashBytes), code) {
		s.audit(ctx, key, purpose, ActionVerify, ip, false)
		return AuthResult{}, ErrOTPInvalid
	}

	// Success: the code is single-use regardless of remaining TTL.
	_ = s.cache.Del(ctx, otpCacheKey(key))
	_ = s.cache.Del(ctx, otpAttemptsCacheKey(key))
	s.audit(ctx, key, purpose, ActionVerify, ip, true)

	var u *User
	var isNewUser bool
	// Doctor promotion is keyed on the phone an application was filed under,
	// so it only applies to a phone identity. An email OTP verifies as a
	// patient; the doctor keeps the phone path, or an admin re-roles them.
	var approvedApp *DoctorApplication
	if s.doctors != nil && phone != "" {
		if app, aerr := s.doctors.ApplicationByPhone(ctx, phone); aerr == nil && app.Status == "approved" {
			approvedApp = &app
		} else if aerr != nil && !errors.Is(aerr, ErrDoctorApplicationNotFound) {
			s.log.Warn().Err(aerr).Msg("doctor application lookup failed; continuing as patient OTP")
		}
	}

	err = database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Look the account up by whichever identity proved ownership. Only
		// that one: an email OTP proves control of the mailbox and says
		// nothing about any phone number, and vice versa, so matching on the
		// other column here would hand one identity's holder the other's
		// account.
		var existing *User
		var ferr error
		if ident.Email != "" {
			existing, ferr = s.repo.FindUserByEmail(ctx, tx, ident.Email)
		} else {
			existing, ferr = s.repo.FindUserByPhone(ctx, tx, phone)
		}
		switch {
		case ferr == nil:
			u = existing
			if approvedApp != nil {
				return s.promoteUserFromApplication(ctx, tx, u, *approvedApp)
			}
		case errors.Is(ferr, ErrUserNotFound):
			// SDD 33.1's verify request is {phone, otp, device_id} only --
			// it carries no "purpose", so verify cannot gate creation on it.
			// Proof of phone ownership (a correctly verified OTP) is what
			// authorises account creation here; find-or-create is
			// unconditional, matching standard passwordless-OTP UX.
			isNewUser = true
			role := RolePatient
			name := ""
			var emailPtr *string
			passwordHash := ""
			if approvedApp != nil {
				role = RoleDoctor
				name = approvedApp.DisplayName
				if approvedApp.Email != "" {
					email := approvedApp.Email
					emailPtr = &email
				}
				passwordHash = approvedApp.PasswordHash
			}
			// An email identity creates an email-only account: phone stays
			// empty, which migration 000005 allows -- it dropped NOT NULL on
			// phone and added the CHECK that one of the two is present.
			if ident.Email != "" && emailPtr == nil {
				email := ident.Email
				emailPtr = &email
			}
			u = &User{
				Phone:        phone,
				Name:         name,
				Email:        emailPtr,
				PasswordHash: passwordHash,
				// English default; the client sets a preference later via
				// PUT /users/me. OTP verify carries no profile fields (SDD
				// 33.1's verify request is {phone, otp, device_id} only).
				Language: LanguageEnglish,
				Role:     role,
				Status:   StatusActive,
			}
			if createErr := s.repo.CreateUser(ctx, tx, u); createErr != nil {
				return createErr
			}
			return s.enqueueRegistered(ctx, tx, u)
		default:
			return ferr
		}
		return nil
	})
	if err != nil {
		return AuthResult{}, err
	}

	if u.Status == StatusSuspended {
		return AuthResult{}, ErrUserSuspended
	}
	if u.Status == StatusDeleted {
		return AuthResult{}, ErrUserDeleted
	}

	if approvedApp != nil {
		if aerr := s.attachApprovedDoctor(ctx, *approvedApp, u); aerr != nil {
			return AuthResult{}, aerr
		}
	}

	if isNewUser {
		s.provisionKeycloak(ctx, u)
	}

	return s.issueSession(ctx, *u, deviceID, uuid.New())
}

func emailAuthCacheKey(email string) string {
	sum := sha256.Sum256([]byte("email-auth:" + email))
	return "auth:email:" + hex.EncodeToString(sum[:])
}

func (s *Service) allowEmailAuth(ctx context.Context, email string) error {
	n, err := s.cache.Incr(ctx, emailAuthCacheKey(email), emailAuthLimitWindow)
	if err != nil {
		return fmt.Errorf("user: email auth rate limit: %w", err)
	}
	if n > emailAuthLimitPerEmail {
		return ErrRateLimited
	}
	return nil
}

func (s *Service) refuseIfClosed(u *User) error {
	if u == nil {
		return ErrUserNotFound
	}
	if u.Status == StatusSuspended {
		return ErrUserSuspended
	}
	if u.Status == StatusDeleted {
		return ErrUserDeleted
	}
	return nil
}

func (s *Service) enqueueRegistered(ctx context.Context, tx pgx.Tx, u *User) error {
	payload := events.UserRegistered{
		UserID:    u.ID,
		Role:      string(u.Role),
		Language:  string(u.Language),
		CreatedAt: u.CreatedAt,
	}
	return s.outbox.Enqueue(ctx, tx, events.SubjectUserRegistered, u.ID.String(), payload)
}

// RegisterEmail creates a patient account with email and password and mints
// a session. Doctors are never self-registered this way; they are promoted
// (or seeded) and then sign in with the same email/password endpoints.
func (s *Service) RegisterEmail(ctx context.Context, rawEmail, password, name, deviceID string) (AuthResult, error) {
	email := NormalizeEmail(rawEmail)
	if !validEmail(email) {
		return AuthResult{}, ErrInvalidEmail
	}
	if !validPassword(password) {
		return AuthResult{}, ErrInvalidPassword
	}
	if err := s.allowEmailAuth(ctx, email); err != nil {
		return AuthResult{}, err
	}

	hash, err := hashPassword(password)
	if err != nil {
		return AuthResult{}, fmt.Errorf("user: hash password: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = emailLocalPart(email)
	}

	var u *User
	err = database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ferr := s.repo.FindUserByEmail(ctx, tx, email)
		switch {
		case ferr == nil && existing != nil:
			return ErrEmailTaken
		case !errors.Is(ferr, ErrUserNotFound):
			return ferr
		}
		u = &User{
			Email:        &email,
			Name:         name,
			Language:     LanguageEnglish,
			Role:         RolePatient,
			Status:       StatusActive,
			PasswordHash: hash,
		}
		if createErr := s.repo.CreateUser(ctx, tx, u); createErr != nil {
			return createErr
		}
		return s.enqueueRegistered(ctx, tx, u)
	})
	if err != nil {
		return AuthResult{}, err
	}

	s.provisionKeycloak(ctx, u)
	return s.issueSession(ctx, *u, deviceID, uuid.New())
}

// LoginEmail authenticates an existing account by email and password. The
// error for "no such email" and "wrong password" is the same, so the endpoint
// cannot be used to enumerate registered addresses.
//
// An approved doctor application with no user row yet is activated here:
// admin approval used to leave the account unusable until a phone OTP ran.
func (s *Service) LoginEmail(ctx context.Context, rawEmail, password, deviceID string) (AuthResult, error) {
	email := NormalizeEmail(rawEmail)
	if !validEmail(email) {
		return AuthResult{}, ErrInvalidEmail
	}
	if password == "" {
		return AuthResult{}, ErrInvalidCredentials
	}
	if err := s.allowEmailAuth(ctx, email); err != nil {
		return AuthResult{}, err
	}

	u, err := s.repo.FindUserByEmail(ctx, s.repo.Pool(), email)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return AuthResult{}, err
	}

	app, hasApp := s.lookupDoctorApplicationByEmail(ctx, email)

	if errors.Is(err, ErrUserNotFound) {
		if !hasApp || !applicationReadyToActivate(app) {
			_ = consumePasswordCheck("", password)
			return AuthResult{}, ErrInvalidCredentials
		}
		if !consumePasswordCheck(app.PasswordHash, password) {
			return AuthResult{}, ErrInvalidCredentials
		}
		u, aerr := s.ensureDoctorAccount(ctx, app)
		if aerr != nil {
			return AuthResult{}, loginDoctorAccountError(aerr)
		}
		return s.issueSession(ctx, *u, deviceID, uuid.New())
	}

	if err := s.refuseIfClosed(u); err != nil {
		return AuthResult{}, err
	}

	hash, herr := s.repo.GetPasswordHash(ctx, s.repo.Pool(), u.ID)
	if herr != nil {
		return AuthResult{}, herr
	}
	if hash == "" && hasApp && app.PasswordHash != "" {
		hash = app.PasswordHash
	}
	if !consumePasswordCheck(hash, password) {
		return AuthResult{}, ErrInvalidCredentials
	}
	if hasApp && applicationReadyToActivate(app) {
		if _, aerr := s.ensureDoctorAccount(ctx, app); aerr != nil {
			return AuthResult{}, loginDoctorAccountError(aerr)
		}
		fresh, ferr := s.repo.FindUserByEmail(ctx, s.repo.Pool(), email)
		if ferr == nil {
			u = fresh
		}
	}
	return s.issueSession(ctx, *u, deviceID, uuid.New())
}

// LoginGoogle verifies a Google ID token and find-or-creates a patient
// account. An existing user with the same verified email is linked rather
// than duplicated, so a doctor who already has that email on their profile
// can sign in with Google without becoming a second (patient) identity.
func (s *Service) LoginGoogle(ctx context.Context, idToken, deviceID string, allowCreate bool) (AuthResult, error) {
	if s.google == nil {
		return AuthResult{}, ErrGoogleDisabled
	}
	ident, err := s.google.Verify(ctx, idToken)
	if err != nil {
		return AuthResult{}, err
	}
	if err := s.allowEmailAuth(ctx, ident.Email); err != nil {
		return AuthResult{}, err
	}

	var u *User
	var isNew bool
	now := time.Now().UTC()
	err = database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ferr := s.repo.FindUserByGoogleSub(ctx, tx, ident.Sub)
		if ferr == nil {
			u = existing
			return nil
		}
		if !errors.Is(ferr, ErrUserNotFound) {
			return ferr
		}

		byEmail, eerr := s.repo.FindUserByEmail(ctx, tx, ident.Email)
		switch {
		case eerr == nil:
			if byEmail.GoogleSub != nil && *byEmail.GoogleSub != ident.Sub {
				return ErrGoogleTaken
			}
			if err := s.repo.LinkGoogle(ctx, tx, byEmail.ID, ident.Sub, now); err != nil {
				return err
			}
			linked, lerr := s.repo.FindUserByID(ctx, tx, byEmail.ID)
			if lerr != nil {
				return lerr
			}
			u = linked
			return nil
		case !errors.Is(eerr, ErrUserNotFound):
			return eerr
		}

		if !allowCreate {
			return ErrUserNotFound
		}

		isNew = true
		email := ident.Email
		sub := ident.Sub
		u = &User{
			Email:           &email,
			Name:            ident.Name,
			Language:        LanguageEnglish,
			Role:            RolePatient,
			Status:          StatusActive,
			GoogleSub:       &sub,
			EmailVerifiedAt: &now,
		}
		if createErr := s.repo.CreateUser(ctx, tx, u); createErr != nil {
			return createErr
		}
		return s.enqueueRegistered(ctx, tx, u)
	})
	if err != nil {
		return AuthResult{}, err
	}
	if err := s.refuseIfClosed(u); err != nil {
		return AuthResult{}, err
	}
	if isNew {
		s.provisionKeycloak(ctx, u)
	}
	return s.issueSession(ctx, *u, deviceID, uuid.New())
}

// SetPassword sets or replaces the account password. OTP-only and Google-only
// accounts have no current password, so current may be empty on first set.
func (s *Service) SetPassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	if !validPassword(next) {
		return ErrInvalidPassword
	}
	u, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return err
	}
	if err := s.refuseIfClosed(u); err != nil {
		return err
	}
	hash, err := s.repo.GetPasswordHash(ctx, s.repo.Pool(), userID)
	if err != nil {
		return err
	}
	if hash != "" && !passwordMatches(hash, current) {
		return ErrInvalidCredentials
	}
	newHash, err := hashPassword(next)
	if err != nil {
		return fmt.Errorf("user: hash password: %w", err)
	}
	return s.repo.SetPasswordHash(ctx, s.repo.Pool(), userID, newHash)
}

// provisionKeycloak mirrors the new user into Keycloak, best-effort. Failure
// is logged and swallowed: the OTP login path must work even when Keycloak
// is unreachable, per AGENT-BRIEF. A future reconciliation job (not in this
// pass) would retry users with keycloak_id still NULL.
func (s *Service) provisionKeycloak(ctx context.Context, u *User) {
	kcID, err := s.keycloak.CreateUser(ctx, *u)
	if err != nil {
		s.log.Warn().Err(err).Str("user_id", logger.MaskID(u.ID.String())).Msg("keycloak user provisioning failed; continuing without it")
		return
	}
	if err := s.repo.SetKeycloakID(ctx, s.repo.Pool(), u.ID, kcID); err != nil {
		s.log.Warn().Err(err).Msg("failed to persist keycloak id")
		return
	}
	u.KeycloakID = &kcID
}

func (s *Service) resolveDoctorID(ctx context.Context, userID uuid.UUID) uuid.UUID {
	if s.doctors == nil {
		return uuid.Nil
	}
	docID, err := s.doctors.DoctorIDByUserID(ctx, userID)
	if err != nil {
		s.log.Debug().Err(err).Str("user_id", userID.String()).Msg("user: could not resolve doctor profile id for token")
		return uuid.Nil
	}
	return docID
}

// issueSession mints a fresh access/refresh pair, starting a new rotation
// family.
func (s *Service) issueSession(ctx context.Context, u User, deviceID string, familyID uuid.UUID) (AuthResult, error) {
	var docID uuid.UUID
	if u.Role == RoleDoctor {
		docID = s.resolveDoctorID(ctx, u.ID)
	}
	access, accessExp, err := s.tokens.IssueAccessToken(u, docID)
	if err != nil {
		return AuthResult{}, err
	}
	raw, hash, err := NewOpaqueRefreshToken()
	if err != nil {
		return AuthResult{}, err
	}
	rt := &RefreshToken{
		UserID:    u.ID,
		FamilyID:  familyID,
		TokenHash: hash,
		ExpiresAt: time.Now().UTC().Add(RefreshTokenTTL),
	}
	if deviceID != "" {
		h := hashDeviceID(deviceID)
		rt.DeviceIDHash = &h
	}
	if err := s.repo.CreateRefreshToken(ctx, s.repo.Pool(), rt); err != nil {
		return AuthResult{}, err
	}
	return AuthResult{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     raw,
		RefreshExpiresAt: rt.ExpiresAt,
		User:             u,
	}, nil
}

// Refresh rotates a refresh token: the presented token is invalidated and a
// new one issued in the same family. Presenting a token that was already
// rotated (i.e. already revoked with a replaced_by) is treated as reuse --
// the standard signal that a refresh token has been stolen -- and revokes
// the entire session family, forcing every device on that lineage to log in
// again. This matters most for a health app, where a stolen session could
// expose consultation history.
func (s *Service) Refresh(ctx context.Context, rawToken, deviceID string) (AuthResult, error) {
	hash := HashRefreshToken(rawToken)

	var result AuthResult
	// outcome carries a *business* rejection (reuse detected, expired,
	// suspended...) that must still be reported to the caller after the
	// transaction commits. It is deliberately never returned directly from
	// the InTx closure below: database.InTx rolls back on any non-nil
	// error, and a rollback would undo the very revocation these branches
	// exist to persist (e.g. RevokeFamily on reuse detection). Returning nil
	// from the closure lets the side effect commit; outcome is checked once
	// InTx has returned successfully.
	var outcome error

	err := database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		rt, err := s.repo.FindRefreshTokenByHashForUpdate(ctx, tx, hash)
		if errors.Is(err, ErrNotFound) {
			outcome = ErrRefreshInvalid
			return nil
		}
		if err != nil {
			return err
		}

		if rt.RevokedAt != nil && !recentlyRotated(rt) {
			if _, revErr := s.repo.RevokeFamily(ctx, tx, rt.FamilyID); revErr != nil {
				return revErr
			}
			s.log.Warn().Str("user_id", logger.MaskID(rt.UserID.String())).Msg("refresh token reuse detected; session family revoked")
			outcome = ErrRefreshReused
			return nil
		}
		if rt.RevokedAt == nil && time.Now().UTC().After(rt.ExpiresAt) {
			if revErr := s.repo.RevokeRefreshToken(ctx, tx, rt.ID, nil); revErr != nil {
				return revErr
			}
			outcome = ErrRefreshExpired
			return nil
		}

		u, err := s.repo.FindUserByID(ctx, tx, rt.UserID)
		if err != nil {
			return err
		}
		if u.Status == StatusSuspended {
			outcome = ErrUserSuspended
			return nil
		}
		if u.Status == StatusDeleted {
			outcome = ErrUserDeleted
			return nil
		}

		var docID uuid.UUID
		if u.Role == RoleDoctor {
			docID = s.resolveDoctorID(ctx, u.ID)
		}
		access, accessExp, err := s.tokens.IssueAccessToken(*u, docID)
		if err != nil {
			return err
		}
		newRaw, newHash, err := NewOpaqueRefreshToken()
		if err != nil {
			return err
		}
		newRT := &RefreshToken{
			UserID:       u.ID,
			FamilyID:     rt.FamilyID,
			TokenHash:    newHash,
			DeviceIDHash: rt.DeviceIDHash,
			ExpiresAt:    time.Now().UTC().Add(RefreshTokenTTL),
		}
		if deviceID != "" {
			h := hashDeviceID(deviceID)
			newRT.DeviceIDHash = &h
		}
		if err := s.repo.CreateRefreshToken(ctx, tx, newRT); err != nil {
			return err
		}
		if err := s.repo.RevokeRefreshToken(ctx, tx, rt.ID, &newRT.ID); err != nil {
			return err
		}

		result = AuthResult{
			AccessToken:      access,
			AccessExpiresAt:  accessExp,
			RefreshToken:     newRaw,
			RefreshExpiresAt: newRT.ExpiresAt,
			User:             *u,
		}
		return nil
	})
	if err != nil {
		return AuthResult{}, err
	}
	if outcome != nil {
		return AuthResult{}, outcome
	}
	return result, nil
}

func recentlyRotated(rt *RefreshToken) bool {
	return rt.RevokedAt != nil && rt.ReplacedBy != nil && time.Since(*rt.RevokedAt) < RefreshReuseGrace
}

// Logout revokes exactly the presented session. It is idempotent: revoking a
// token that is already gone or already revoked is not an error, since the
// end state the client wants (this session is dead) already holds.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	hash := HashRefreshToken(rawToken)
	rt, err := s.repo.FindRefreshTokenByHash(ctx, s.repo.Pool(), hash)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.repo.RevokeRefreshToken(ctx, s.repo.Pool(), rt.ID, nil)
}

// LogoutAll revokes every active session for userID -- "log out everywhere",
// used from settings and as part of account deletion.
func (s *Service) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	_, err := s.repo.RevokeAllForUser(ctx, s.repo.Pool(), userID)
	return err
}

// audit writes one line to the otp_attempts table. It never returns an error
// to the caller: a failed audit write must not block the auth flow it is
// observing, but it is logged loudly because a broken audit trail is itself
// an incident.
func (s *Service) audit(ctx context.Context, phone string, purpose OTPPurpose, action OTPAction, ip string, success bool) {
	err := s.repo.InsertOTPAttempt(ctx, s.repo.Pool(), OTPAttempt{
		Phone: phone, Purpose: purpose, Action: action, IP: ip, Success: success,
	})
	if err != nil {
		s.log.Error().Err(err).Msg("failed to write otp audit row")
	}
}

// ---------------------------------------------------------------- profile --

// GetProfile returns the caller's own record.
func (s *Service) GetProfile(ctx context.Context, userID uuid.UUID) (*User, error) {
	u, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	if u.DeletedAt != nil {
		return nil, ErrUserNotFound
	}
	return u, nil
}

// UpdateProfileInput carries the editable self-service fields.
type UpdateProfileInput struct {
	Name        string
	Email       *string
	Phone       *string
	Address     *string
	DateOfBirth *time.Time
	ClearDOB    bool
	Language    Language
	Version     int
}

// ApplyProfilePatch overlays the requested edits onto an existing user. Fields
// left nil on the input are unchanged, so a doctor saving only their email
// cannot wipe a patient's address, and the reverse.
func ApplyProfilePatch(u *User, in UpdateProfileInput) {
	u.Name = strings.TrimSpace(in.Name)
	u.Language = in.Language
	u.Version = in.Version
	if in.Email != nil {
		email := strings.TrimSpace(*in.Email)
		if email == "" {
			u.Email = nil
		} else {
			u.Email = &email
		}
	}
	if in.Phone != nil {
		u.Phone = strings.TrimSpace(*in.Phone)
	}
	if in.Address != nil {
		u.Address = strings.TrimSpace(*in.Address)
	}
	if in.ClearDOB {
		u.DateOfBirth = nil
	} else if in.DateOfBirth != nil {
		u.DateOfBirth = in.DateOfBirth
	}
}

func hasLoginIdentity(u *User) bool {
	if u.Phone != "" {
		return true
	}
	if u.Email != nil && *u.Email != "" {
		return true
	}
	return u.GoogleSub != nil && *u.GoogleSub != ""
}

// UpdateProfile applies an edit under optimistic locking; the caller must
// supply the version they last observed.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, in UpdateProfileInput) (*User, error) {
	existing, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	if existing.DeletedAt != nil {
		return nil, ErrUserNotFound
	}
	ApplyProfilePatch(existing, in)
	if !hasLoginIdentity(existing) {
		return nil, ErrNoLoginIdentity
	}
	if err := s.repo.UpdateProfile(ctx, s.repo.Pool(), existing); err != nil {
		return nil, err
	}
	return s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
}

// SetProfilePhoto validates and stores a self-service avatar.
func (s *Service) SetProfilePhoto(ctx context.Context, userID uuid.UUID, filename string, data []byte) (*User, error) {
	if len(data) == 0 {
		return nil, ErrInvalidProfilePhoto
	}
	if len(data) > MaxProfilePhotoBytes {
		return nil, ErrProfilePhotoTooLarge
	}
	ext := ProfilePhotoExtension(filename)
	sniffed := SniffProfilePhotoContentType(data)
	if !IsAllowedProfilePhoto(ext, sniffed) {
		return nil, ErrInvalidProfilePhoto
	}
	existing, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	if existing.DeletedAt != nil {
		return nil, ErrUserNotFound
	}
	if _, err := s.repo.SetProfilePhoto(ctx, s.repo.Pool(), userID, data, sniffed); err != nil {
		return nil, err
	}
	return s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
}

// GetProfilePhoto returns the raw avatar bytes for streaming.
func (s *Service) GetProfilePhoto(ctx context.Context, userID uuid.UUID) (*ProfilePhoto, error) {
	existing, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	if existing.DeletedAt != nil {
		return nil, ErrUserNotFound
	}
	photo, err := s.repo.GetProfilePhoto(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	return photo, nil
}

// ClearProfilePhoto removes the caller's avatar.
func (s *Service) ClearProfilePhoto(ctx context.Context, userID uuid.UUID) (*User, error) {
	existing, err := s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
	if err != nil {
		return nil, err
	}
	if existing.DeletedAt != nil {
		return nil, ErrUserNotFound
	}
	if err := s.repo.ClearProfilePhoto(ctx, s.repo.Pool(), userID); err != nil {
		return nil, err
	}
	return s.repo.FindUserByID(ctx, s.repo.Pool(), userID)
}

// DeleteAccount soft-deletes the account and schedules PDPA erasure after
// erasureGracePeriod, revoking every active session in the same transaction
// so deletion takes effect immediately even though the data itself survives
// the grace window.
func (s *Service) DeleteAccount(ctx context.Context, userID uuid.UUID) error {
	erasureDue := time.Now().UTC().Add(erasureGracePeriod)
	return database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.SoftDeleteUser(ctx, tx, userID, erasureDue); err != nil {
			return err
		}
		_, err := s.repo.RevokeAllForUser(ctx, tx, userID)
		return err
	})
}

// AnonymizeDue runs the PDPA erasure reaper for one batch. It is exported so
// both the background worker (cmd/server/main.go) and a test can drive it
// directly.
func (s *Service) AnonymizeDue(ctx context.Context, batchSize int) (int64, error) {
	var n int64
	err := database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		n, err = s.repo.AnonymizeDueUsers(ctx, tx, time.Now().UTC(), batchSize)
		return err
	})
	return n, err
}

// ----------------------------------------------------------------- family --

// FamilyMemberInput carries the editable fields of a dependant profile.
type FamilyMemberInput struct {
	Name     string
	DOB      time.Time
	Relation FamilyRelation
	NIC      string // plaintext in, keyed-hashed before storage; never returned
}

// AddFamilyMember creates a dependant profile owned by ownerID.
func (s *Service) AddFamilyMember(ctx context.Context, ownerID uuid.UUID, in FamilyMemberInput) (*FamilyMember, error) {
	f := &FamilyMember{OwnerUserID: ownerID, Name: in.Name, DOB: in.DOB, Relation: in.Relation}
	if in.NIC != "" {
		h, v, err := s.hashNIC(in.NIC)
		if err != nil {
			return nil, err
		}
		f.NICHash, f.NICHashVersion = &h, &v
	}
	if err := s.repo.CreateFamilyMember(ctx, s.repo.Pool(), f); err != nil {
		return nil, err
	}
	return f, nil
}

// ListFamilyMembers returns ownerID's dependants.
func (s *Service) ListFamilyMembers(ctx context.Context, ownerID uuid.UUID) ([]FamilyMember, error) {
	return s.repo.ListFamilyMembers(ctx, s.repo.Pool(), ownerID)
}

// getOwnedFamilyMember loads a dependant and enforces that it belongs to
// ownerID. A mismatch is reported identically to "not found" (ErrForbidden
// maps to the same 404-shaped outcome at the handler for reads, 403 for
// writes -- see handler.go) so a caller cannot use response codes to
// enumerate another user's family member ids.
func (s *Service) getOwnedFamilyMember(ctx context.Context, ownerID, memberID uuid.UUID) (*FamilyMember, error) {
	f, err := s.repo.GetFamilyMember(ctx, s.repo.Pool(), memberID)
	if err != nil {
		return nil, err
	}
	if f.OwnerUserID != ownerID {
		return nil, ErrForbidden
	}
	return f, nil
}

// UpdateFamilyMember edits a dependant, verifying ownership first.
func (s *Service) UpdateFamilyMember(ctx context.Context, ownerID, memberID uuid.UUID, in FamilyMemberInput, version int) (*FamilyMember, error) {
	existing, err := s.getOwnedFamilyMember(ctx, ownerID, memberID)
	if err != nil {
		return nil, err
	}
	f := &FamilyMember{ID: existing.ID, Name: in.Name, DOB: in.DOB, Relation: in.Relation, Version: version}
	if in.NIC != "" {
		h, v, herr := s.hashNIC(in.NIC)
		if herr != nil {
			return nil, herr
		}
		f.NICHash, f.NICHashVersion = &h, &v
	} else {
		// Carry BOTH forward, never just the digest: a digest whose version
		// was dropped is a value nothing can verify again, and the database
		// CHECK added in migration 000004 refuses the pair anyway.
		f.NICHash, f.NICHashVersion = existing.NICHash, existing.NICHashVersion
	}
	if err := s.repo.UpdateFamilyMember(ctx, s.repo.Pool(), f); err != nil {
		return nil, err
	}
	return f, nil
}

// DeleteFamilyMember removes a dependant, verifying ownership first.
func (s *Service) DeleteFamilyMember(ctx context.Context, ownerID, memberID uuid.UUID) error {
	if _, err := s.getOwnedFamilyMember(ctx, ownerID, memberID); err != nil {
		return err
	}
	return s.repo.SoftDeleteFamilyMember(ctx, s.repo.Pool(), memberID, ownerID)
}

// --------------------------------------------------------------- consents --

// AddConsent appends one entry to the caller's PDPA/GDPR consent ledger.
func (s *Service) AddConsent(ctx context.Context, userID uuid.UUID, kind ConsentKind, version string, granted bool) (*Consent, error) {
	c := &Consent{UserID: userID, Kind: kind, Version: version, Granted: granted}
	if err := s.repo.InsertConsent(ctx, s.repo.Pool(), c); err != nil {
		return nil, err
	}
	return c, nil
}

// ------------------------------------------------------------------- gRPC --

// GetUser resolves one user for the internal user.v1 gRPC surface.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.FindUserByID(ctx, s.repo.Pool(), id)
}

// MaxUsersBatch caps how many ids one GetUsersBatch call may resolve.
//
// 100 is the platform's existing "one page" ceiling -- httpx.Pagination clamps
// per_page to the same number -- and every legitimate caller of this method is
// resolving the ids visible on one screen. Anything larger is not a page.
const MaxUsersBatch = 100

// ErrBatchTooLarge is returned when a batch lookup exceeds MaxUsersBatch.
var ErrBatchTooLarge = errors.New("user: batch exceeds the maximum size")

// GetUsersBatch resolves many users for the internal user.v1 gRPC surface.
//
// The cap is enforced here as well as at the transport, deliberately. The
// transport check is the one that produces a good error message and the log
// line; this one is the one that still holds when someone adds a second
// caller. A limit that lives only in the handler is a limit that a future
// refactor removes without noticing -- which is how the uncapped version of
// this method existed in the first place.
func (s *Service) GetUsersBatch(ctx context.Context, ids []uuid.UUID) ([]User, error) {
	if len(ids) > MaxUsersBatch {
		return nil, ErrBatchTooLarge
	}
	return s.repo.FindUsersByIDs(ctx, s.repo.Pool(), ids)
}

// ----------------------------------------------------------------- helpers --

func hashDeviceID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// hashNIC returns the keyed digest stored in nic_hash AND the pepper
// generation that produced it. See nic.go for why this is an HMAC with a
// secret pepper rather than bcrypt: the NIC encodes its own birth date, the
// plaintext DOB sits in the same row, and a work factor is worthless against a
// search space of ~10^4.
//
// The two values are returned together, and every caller stores them together.
// A digest without its version is unverifiable the moment the pepper rotates,
// and unverifiable-but-present is the failure mode migration 000004 exists to
// make impossible.
//
// A service with no pepper configured refuses to hash rather than falling back
// to something weaker. Losing the ability to record a dependant's NIC is a
// visible, recoverable outage; writing a recoverable digest of one is neither.
func (s *Service) hashNIC(nic string) (digest string, version int, err error) {
	if s.nic == nil {
		return "", 0, fmt.Errorf("user: NIC hashing is not configured (NIC_HASH_PEPPER)")
	}
	h := s.nic.Hash(nic)
	if h == "" {
		return "", 0, fmt.Errorf("user: NIC contains no usable characters")
	}
	return h, s.nic.Version(), nil
}
