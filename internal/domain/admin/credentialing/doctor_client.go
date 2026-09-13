package credentialing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ApplicationVerifier asks doctor-service to approve/reject a public application.
type ApplicationVerifier interface {
	VerifyApplication(ctx context.Context, applicationID uuid.UUID, approve bool, reason string) error
}

// PendingApplication is the subset of a public apply row the admin queue needs
// to re-project when doctor.application_submitted was lost.
type PendingApplication struct {
	ID              uuid.UUID
	FullName        string
	Email           string
	Phone           string
	SLMCNumber      string
	Specialty       string
	YearsExperience int
	FeeCents        int64
	CreatedAt       time.Time
}

// PendingApplicationSource lists pending public applications from doctor-service.
type PendingApplicationSource interface {
	ListPendingApplications(ctx context.Context) ([]PendingApplication, error)
}

// ErrApplicationNotFound means doctor-service has no application with that id
// (legacy doctor.registered rows still use the event-only approve path).
var ErrApplicationNotFound = errors.New("credentialing: doctor application not found")

// TokenSource mints a mesh Bearer token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// HTTPApplicationVerifier calls doctor-service internal verify.
type HTTPApplicationVerifier struct {
	baseURL string
	tokens  TokenSource
	client  *http.Client
}

func NewHTTPApplicationVerifier(baseURL string, tokens TokenSource) *HTTPApplicationVerifier {
	return &HTTPApplicationVerifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *HTTPApplicationVerifier) VerifyApplication(ctx context.Context, applicationID uuid.UUID, approve bool, reason string) error {
	action := "reject"
	if approve {
		action = "approve"
	}
	body, err := json.Marshal(map[string]string{"action": action, "reason": reason})
	if err != nil {
		return err
	}
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("credentialing: mesh token: %w", err)
	}
	url := c.baseURL + "/api/v1/internal/doctors/applications/" + applicationID.String() + "/verify"
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("credentialing: doctor-service verify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrApplicationNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("credentialing: doctor-service verify returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

type pendingApplicationsEnvelope struct {
	Data []struct {
		ApplicationID   string    `json:"application_id"`
		DisplayName     string    `json:"display_name"`
		Email           string    `json:"email"`
		Phone           string    `json:"phone"`
		SLMCNumber      string    `json:"slmc_number"`
		Specialty       string    `json:"specialty"`
		ExperienceYears int       `json:"experience_years"`
		FeeCents        int64     `json:"fee_cents"`
		CreatedAt       time.Time `json:"created_at"`
	} `json:"data"`
}

// ListPendingApplications implements PendingApplicationSource.
func (c *HTTPApplicationVerifier) ListPendingApplications(ctx context.Context) ([]PendingApplication, error) {
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("credentialing: mesh token: %w", err)
	}
	url := c.baseURL + "/api/v1/internal/doctors/applications/pending?per_page=100&page=1"
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	//nolint:gosec // G704: mesh address from config, not from a request
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("credentialing: doctor-service pending applications: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("credentialing: read pending applications: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("credentialing: doctor-service pending applications returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var env pendingApplicationsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("credentialing: decode pending applications: %w", err)
	}
	out := make([]PendingApplication, 0, len(env.Data))
	for i := range env.Data {
		row := &env.Data[i]
		id, err := uuid.Parse(row.ApplicationID)
		if err != nil {
			continue
		}
		out = append(out, PendingApplication{
			ID: id, FullName: row.DisplayName, Email: row.Email, Phone: row.Phone,
			SLMCNumber: row.SLMCNumber, Specialty: row.Specialty,
			YearsExperience: row.ExperienceYears, FeeCents: row.FeeCents, CreatedAt: row.CreatedAt,
		})
	}
	return out, nil
}
