package user

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Handler is the HTTP boundary for the user domain. It decodes requests,
// calls the service, and encodes responses -- it never runs SQL and never
// embeds a business rule that would need a unit test of its own.
type Handler struct {
	svc    *Service
	tokens *TokenIssuer
	cache  cache.Cache
	log    zerolog.Logger
}

// NewHandler builds the HTTP adapter over the domain service.
func NewHandler(svc *Service, tokens *TokenIssuer, c cache.Cache, log zerolog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, cache: c, log: log}
}

// Routes returns the full /api/v1 subtree this service owns, mountable
// directly: srv.Router.Mount("/api/v1", handler.Routes()).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	r.Route("/auth", func(r chi.Router) {
		// The 3-per-hour-per-phone limit lives in the service layer
		// (SendOTP); this is the *separate* per-IP limit AGENT-BRIEF calls
		// for, reusing the platform's own Cache.Incr-backed limiter so both
		// limits are fixed-window and shared across every replica.
		r.With(middleware.RateLimit(h.cache, middleware.RateLimitConfig{
			Name: "otp_send_ip", Requests: 10, Window: time.Hour,
		}, h.log)).Post("/otp/send", h.SendOTP)

		r.With(middleware.RateLimit(h.cache, middleware.RateLimitConfig{
			Name: "otp_verify_ip", Requests: 30, Window: time.Hour,
		}, h.log)).Post("/otp/verify", h.VerifyOTP)

		r.With(middleware.RateLimit(h.cache, middleware.RateLimitConfig{
			Name: "auth_email_register_ip", Requests: 10, Window: time.Hour,
		}, h.log)).Post("/register/email", h.RegisterEmail)

		r.With(middleware.RateLimit(h.cache, middleware.RateLimitConfig{
			Name: "auth_email_login_ip", Requests: 20, Window: time.Hour,
		}, h.log)).Post("/login/email", h.LoginEmail)

		r.With(middleware.RateLimit(h.cache, middleware.RateLimitConfig{
			Name: "auth_google_ip", Requests: 20, Window: time.Hour,
		}, h.log)).Post("/oauth/google", h.LoginGoogle)

		r.Post("/refresh", h.Refresh)
		r.Post("/logout", h.Logout)
		r.With(h.RequireAuth).Post("/logout-all", h.LogoutAll)
	})

	r.Route("/users/me", func(r chi.Router) {
		r.Use(h.RequireAuth)
		r.Use(middleware.NoStore)

		r.Get("/", h.GetMe)
		r.Put("/", h.UpdateMe)
		r.Put("/password", h.SetPassword)
		r.Delete("/", h.DeleteMe)

		r.Route("/family", func(r chi.Router) {
			r.Get("/", h.ListFamily)
			r.Post("/", h.CreateFamily)
			r.Put("/{id}", h.UpdateFamily)
			r.Delete("/{id}", h.DeleteFamily)
		})

		r.Post("/consents", h.AddConsent)
	})

	return r
}

// -------------------------------------------------------------- principal --

type principalCtxKey struct{}

func withPrincipal(ctx context.Context, p VerifiedPrincipal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext extracts the authenticated caller set by RequireAuth.
func PrincipalFromContext(ctx context.Context) (VerifiedPrincipal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(VerifiedPrincipal)
	return p, ok
}

// RequireAuth verifies the bearer token against this service's own signing
// key -- see TokenIssuer.Verify for why this service does not use the
// platform's JWKS-fetching middleware.Authenticator for its own routes.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		scheme, token, found := strings.Cut(authz, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
			httpx.Error(w, r, httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized,
				"Authorization header must be in 'Bearer <token>' form"))
			return
		}
		p, err := h.tokens.Verify(token)
		if err != nil {
			httpx.Error(w, r, httpx.ErrUnauthorized.WithCause(err))
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// JWKS serves this issuer's public key so any service can verify tokens
// user-service minted, via the standard platform Authenticator pointed at
// this URL.
func (h *Handler) JWKS(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, h.tokens.PublicJWKS())
}

// ------------------------------------------------------------------- DTOs --

type sendOTPRequest struct {
	Phone    string `json:"phone" validate:"required"`
	Purpose  string `json:"purpose" validate:"required,oneof=register login"`
	Language string `json:"language" validate:"omitempty,oneof=en si ta"`
}

type sendOTPResponse struct {
	RequestID         string `json:"request_id"`
	ExpiresIn         int    `json:"expires_in"`
	AttemptsRemaining int    `json:"attempts_remaining"`
}

type verifyOTPRequest struct {
	Phone    string `json:"phone" validate:"required"`
	OTP      string `json:"otp" validate:"required,len=6,numeric"`
	DeviceID string `json:"device_id" validate:"omitempty,max=200"`
	Purpose  string `json:"purpose" validate:"omitempty,oneof=register login"`
}

type authResponse struct {
	AccessToken  string       `json:"access_token"`
	ExpiresIn    int          `json:"expires_in"`
	RefreshToken string       `json:"refresh_token"`
	User         userResponse `json:"user"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
	DeviceID     string `json:"device_id" validate:"omitempty,max=200"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type emailRegisterRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=8,max=72"`
	Name     string `json:"name" validate:"omitempty,max=200"`
	DeviceID string `json:"device_id" validate:"omitempty,max=200"`
}

type emailLoginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
	DeviceID string `json:"device_id" validate:"omitempty,max=200"`
}

type googleLoginRequest struct {
	IDToken       string `json:"id_token" validate:"required"`
	DeviceID      string `json:"device_id" validate:"omitempty,max=200"`
	CreateAccount *bool  `json:"create_account"`
}

type userResponse struct {
	ID          string  `json:"id"`
	Phone       string  `json:"phone"`
	Email       *string `json:"email,omitempty"`
	Name        string  `json:"name"`
	Language    string  `json:"language"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	NoShowCount int     `json:"no_show_count"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	Version     int     `json:"version"`
}

func toUserResponse(u User) userResponse {
	return userResponse{
		ID: u.ID.String(), Phone: u.Phone, Email: u.Email, Name: u.Name,
		Language: string(u.Language), Role: string(u.Role), Status: string(u.Status),
		NoShowCount: u.NoShowCount,
		CreatedAt:   u.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   u.UpdatedAt.Format(time.RFC3339),
		Version:     u.Version,
	}
}

type updateProfileRequest struct {
	Name     string  `json:"name" validate:"required,min=1,max=200"`
	Email    *string `json:"email" validate:"omitempty,email"`
	Language string  `json:"language" validate:"required,oneof=en si ta"`
	Version  int     `json:"version" validate:"gte=0"`
}

type setPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password" validate:"required,min=8,max=72"`
}

type familyMemberRequest struct {
	Name     string `json:"name" validate:"required,min=1,max=200"`
	DOB      string `json:"dob" validate:"required,datetime=2006-01-02"`
	Relation string `json:"relation" validate:"required,oneof=child parent spouse sibling other"`
	NIC      string `json:"nic" validate:"omitempty,max=20"`
	Version  int    `json:"version" validate:"gte=0"`
}

type familyMemberResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DOB      string `json:"dob"`
	Relation string `json:"relation"`
	HasNIC   bool   `json:"has_nic"`
	Version  int    `json:"version"`
}

func toFamilyResponse(f FamilyMember) familyMemberResponse {
	return familyMemberResponse{
		ID: f.ID.String(), Name: f.Name, DOB: f.DOB.Format("2006-01-02"),
		Relation: string(f.Relation), HasNIC: f.NICHash != nil, Version: f.Version,
	}
}

type consentRequest struct {
	Kind    string `json:"kind" validate:"required,oneof=pdpa terms marketing telehealth"`
	Version string `json:"version" validate:"required"`
	Granted *bool  `json:"granted" validate:"required"`
}

type consentResponse struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Granted   bool   `json:"granted"`
	CreatedAt string `json:"created_at"`
}

// ------------------------------------------------------------ auth routes --

func (h *Handler) SendOTP(w http.ResponseWriter, r *http.Request) {
	var req sendOTPRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	lang := Language(req.Language)
	if lang == "" {
		lang = LanguageEnglish
	}
	res, err := h.svc.SendOTP(r.Context(), req.Phone, OTPPurpose(req.Purpose), lang, requestIP(r))
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, sendOTPResponse{
		RequestID: res.RequestID, ExpiresIn: int(res.ExpiresIn.Seconds()), AttemptsRemaining: res.AttemptsRemaining,
	})
}

func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req verifyOTPRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	purpose := OTPPurpose(req.Purpose)
	if purpose == "" {
		purpose = PurposeLogin
	}
	res, err := h.svc.VerifyOTP(r.Context(), req.Phone, req.OTP, req.DeviceID, purpose, requestIP(r))
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, authResponse{
		AccessToken: res.AccessToken, ExpiresIn: int(AccessTokenTTL.Seconds()),
		RefreshToken: res.RefreshToken, User: toUserResponse(res.User),
	})
}

func (h *Handler) RegisterEmail(w http.ResponseWriter, r *http.Request) {
	var req emailRegisterRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.RegisterEmail(r.Context(), req.Email, req.Password, req.Name, req.DeviceID)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.Created(w, r, authResponse{
		AccessToken: res.AccessToken, ExpiresIn: int(AccessTokenTTL.Seconds()),
		RefreshToken: res.RefreshToken, User: toUserResponse(res.User),
	})
}

func (h *Handler) LoginEmail(w http.ResponseWriter, r *http.Request) {
	var req emailLoginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.LoginEmail(r.Context(), req.Email, req.Password, req.DeviceID)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	h.writeAuth(w, r, res)
}

func (h *Handler) LoginGoogle(w http.ResponseWriter, r *http.Request) {
	var req googleLoginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.LoginGoogle(r.Context(), req.IDToken, req.DeviceID, req.CreateAccount == nil || *req.CreateAccount)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	h.writeAuth(w, r, res)
}

func (h *Handler) writeAuth(w http.ResponseWriter, r *http.Request, res AuthResult) {
	httpx.OK(w, r, authResponse{
		AccessToken: res.AccessToken, ExpiresIn: int(AccessTokenTTL.Seconds()),
		RefreshToken: res.RefreshToken, User: toUserResponse(res.User),
	})
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.svc.Refresh(r.Context(), req.RefreshToken, req.DeviceID)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, authResponse{
		AccessToken: res.AccessToken, ExpiresIn: int(AccessTokenTTL.Seconds()),
		RefreshToken: res.RefreshToken, User: toUserResponse(res.User),
	})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) LogoutAll(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	if err := h.svc.LogoutAll(r.Context(), p.UserID); err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.NoContent(w, r)
}

// ----------------------------------------------------------- users/me --

func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	u, err := h.svc.GetProfile(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, toUserResponse(*u))
}

func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	var req updateProfileRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	u, err := h.svc.UpdateProfile(r.Context(), p.UserID, UpdateProfileInput{
		Name: req.Name, Email: req.Email, Language: Language(req.Language), Version: req.Version,
	})
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, toUserResponse(*u))
}

func (h *Handler) SetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	var req setPasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.SetPassword(r.Context(), p.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) DeleteMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	if err := h.svc.DeleteAccount(r.Context(), p.UserID); err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.NoContent(w, r)
}

// ------------------------------------------------------- users/me/family --

func (h *Handler) ListFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	members, err := h.svc.ListFamilyMembers(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	out := make([]familyMemberResponse, 0, len(members))
	// Indexed rather than ranged by value: FamilyMember is 168 bytes.
	for i := range members {
		out = append(out, toFamilyResponse(members[i]))
	}
	httpx.OK(w, r, out)
}

func (h *Handler) CreateFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req familyMemberRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	dob, err := time.Parse("2006-01-02", req.DOB)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "dob must be YYYY-MM-DD"))
		return
	}
	f, err := h.svc.AddFamilyMember(r.Context(), p.UserID, FamilyMemberInput{
		Name: req.Name, DOB: dob, Relation: FamilyRelation(req.Relation), NIC: req.NIC,
	})
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.Created(w, r, toFamilyResponse(*f))
}

func (h *Handler) UpdateFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req familyMemberRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	dob, err := time.Parse("2006-01-02", req.DOB)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "dob must be YYYY-MM-DD"))
		return
	}
	f, err := h.svc.UpdateFamilyMember(r.Context(), p.UserID, id, FamilyMemberInput{
		Name: req.Name, DOB: dob, Relation: FamilyRelation(req.Relation), NIC: req.NIC,
	}, req.Version)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.OK(w, r, toFamilyResponse(*f))
}

func (h *Handler) DeleteFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteFamilyMember(r.Context(), p.UserID, id); err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.NoContent(w, r)
}

// ---------------------------------------------------- users/me/consents --

func (h *Handler) AddConsent(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var req consentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := h.svc.AddConsent(r.Context(), p.UserID, ConsentKind(req.Kind), req.Version, *req.Granted)
	if err != nil {
		httpx.Error(w, r, mapError(err))
		return
	}
	httpx.Created(w, r, consentResponse{
		ID: c.ID.String(), Kind: string(c.Kind), Version: c.Version, Granted: c.Granted,
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
	})
}

// -------------------------------------------------------------- helpers --

// requestIP resolves the caller address for the OTP audit trail. Trusting
// X-Forwarded-For is safe only because every deployment terminates at an
// ingress that overwrites it -- identical assumption to
// internal/platform/middleware's own clientIP.
func requestIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	return r.RemoteAddr
}

// mapError translates a domain error into the platform's httpx error
// taxonomy. Unmapped errors pass through unchanged; httpx.Error flattens any
// non-APIError to a generic 500 so an internal detail can never leak to a
// client by omission here.
func mapError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidPhone):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "invalid phone number")
	case errors.Is(err, ErrInvalidEmail):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "invalid email")
	case errors.Is(err, ErrInvalidPassword):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "password must be 8–72 characters")
	case errors.Is(err, ErrInvalidCredentials):
		return httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid email or password")
	case errors.Is(err, ErrGoogleDisabled):
		return httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "google sign-in is not configured")
	case errors.Is(err, ErrGoogleTokenInvalid):
		return httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "google sign-in failed")
	case errors.Is(err, ErrGoogleEmailUnverified):
		return httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "google email is not verified")
	case errors.Is(err, ErrRateLimited):
		return httpx.ErrRateLimited
	case errors.Is(err, ErrOTPLocked):
		return httpx.NewError(http.StatusTooManyRequests, httpx.CodeRateLimited, "too many attempts; request a new code")
	case errors.Is(err, ErrOTPExpired):
		return httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "otp expired or was never requested")
	case errors.Is(err, ErrOTPInvalid):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "otp does not match")
	case errors.Is(err, ErrUserNotFound), errors.Is(err, ErrFamilyNotFound), errors.Is(err, ErrNotFound):
		return httpx.ErrNotFound
	case errors.Is(err, ErrUserSuspended):
		return httpx.NewError(http.StatusForbidden, httpx.CodeForbidden, "account suspended")
	case errors.Is(err, ErrUserDeleted):
		return httpx.ErrNotFound
	case errors.Is(err, ErrRefreshInvalid), errors.Is(err, ErrRefreshExpired), errors.Is(err, ErrRefreshReused):
		return httpx.ErrUnauthorized
	case errors.Is(err, ErrForbidden):
		return httpx.ErrForbidden
	case errors.Is(err, ErrEmailTaken), errors.Is(err, ErrPhoneTaken), errors.Is(err, ErrGoogleTaken):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict, "already in use")
	case errors.Is(err, ErrOptimisticLock):
		return httpx.ErrConflict
	default:
		return err
	}
}
