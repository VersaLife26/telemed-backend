package user

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Google's public tokeninfo endpoint. Named *TokenInfo*, which is why gosec
// reads it as a credential; it is a URL and carries no secret.
const googleTokenInfoURL = "https://oauth2.googleapis.com/tokeninfo" //nolint:gosec // G101: a public URL, not a credential

// googleHTTPDoer is the slice of http.Client we need, so tests can stub the
// tokeninfo round trip without standing up Google.
type googleHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GoogleTokenInfoVerifier validates a Google ID token by asking Google's
// tokeninfo endpoint and checking audience + email_verified. One HTTP call
// per login is the cost of not pinning Google's JWKS at boot (which would
// make a Google outage a boot failure).
type GoogleTokenInfoVerifier struct {
	clientID string
	client   googleHTTPDoer
}

// NewGoogleTokenInfoVerifier builds a verifier for audience clientID.
// client may be nil, in which case a short-timeout http.Client is used.
func NewGoogleTokenInfoVerifier(clientID string, client googleHTTPDoer) *GoogleTokenInfoVerifier {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &GoogleTokenInfoVerifier{clientID: strings.TrimSpace(clientID), client: client}
}

type googleTokenInfo struct {
	Aud           string `json:"aud"`
	Iss           string `json:"iss"`
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	Error         string `json:"error"`
	ErrorDesc     string `json:"error_description"`
}

func (v *GoogleTokenInfoVerifier) Verify(ctx context.Context, idToken string) (GoogleIdentity, error) {
	if v == nil || v.clientID == "" {
		return GoogleIdentity{}, ErrGoogleDisabled
	}
	idToken = strings.TrimSpace(idToken)
	if idToken == "" {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}

	u, err := url.Parse(googleTokenInfoURL)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("user: google tokeninfo url: %w", err)
	}
	q := u.Query()
	q.Set("id_token", idToken)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("user: google tokeninfo request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("user: google tokeninfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("user: google tokeninfo read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}

	var info googleTokenInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}
	if info.Error != "" || info.Sub == "" {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}
	if info.Aud != v.clientID {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}
	switch info.Iss {
	case "https://accounts.google.com", "accounts.google.com":
	default:
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}

	email := NormalizeEmail(info.Email)
	if email == "" {
		return GoogleIdentity{}, ErrGoogleTokenInvalid
	}
	verified := googleBool(info.EmailVerified)
	if !verified {
		return GoogleIdentity{}, ErrGoogleEmailUnverified
	}
	name := strings.TrimSpace(info.Name)
	if name == "" {
		name = emailLocalPart(email)
	}
	return GoogleIdentity{
		Sub:           info.Sub,
		Email:         email,
		EmailVerified: true,
		Name:          name,
	}, nil
}

func googleBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	default:
		return false
	}
}

func emailLocalPart(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return email
	}
	return email[:at]
}
