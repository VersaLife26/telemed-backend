package user

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DoctorApplications looks up and activates approved public doctor applications.
//
// Implemented over doctor-service's internal HTTP surface with a mesh service
// token. Nil on Service means OTP behaves as before (patient find-or-create).
type DoctorApplications interface {
	ApplicationByPhone(ctx context.Context, phone string) (DoctorApplication, error)
	Attach(ctx context.Context, applicationID, userID uuid.UUID, email string) error
}

// DoctorApplication is the subset of the doctor-service application response
// OTP verify needs.
type DoctorApplication struct {
	ID           uuid.UUID
	Status       string
	DisplayName  string
	Email        string
	Phone        string
	PasswordHash string
}

// ErrDoctorApplicationNotFound is returned when no open application exists.
var ErrDoctorApplicationNotFound = fmt.Errorf("user: doctor application not found")

// TokenSource mints a mesh Bearer token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// HTTPDoctorApplications talks to doctor-service over the mesh.
type HTTPDoctorApplications struct {
	baseURL string
	tokens  TokenSource
	client  *http.Client
}

// NewHTTPDoctorApplications builds a client. baseURL is e.g. http://doctor-service:8082.
func NewHTTPDoctorApplications(baseURL string, tokens TokenSource) *HTTPDoctorApplications {
	return &HTTPDoctorApplications{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		client:  &http.Client{Timeout: 8 * time.Second},
	}
}

type envelopeData[T any] struct {
	Data T `json:"data"`
}

type applicationDTO struct {
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
	DisplayName   string `json:"display_name"`
	Email         string `json:"email"`
	Phone         string `json:"phone"`
	PasswordHash  string `json:"password_hash"`
}

func (c *HTTPDoctorApplications) ApplicationByPhone(ctx context.Context, phone string) (DoctorApplication, error) {
	path := c.baseURL + "/api/v1/internal/doctors/applications/by-phone/" + url.PathEscape(phone)
	var dto applicationDTO
	if err := c.getJSON(ctx, path, &dto); err != nil {
		return DoctorApplication{}, err
	}
	id, err := uuid.Parse(dto.ApplicationID)
	if err != nil {
		return DoctorApplication{}, fmt.Errorf("user: doctor application id: %w", err)
	}
	return DoctorApplication{
		ID: id, Status: dto.Status, DisplayName: dto.DisplayName,
		Email: dto.Email, Phone: dto.Phone, PasswordHash: dto.PasswordHash,
	}, nil
}

func (c *HTTPDoctorApplications) Attach(ctx context.Context, applicationID, userID uuid.UUID, email string) error {
	path := c.baseURL + "/api/v1/internal/doctors/applications/" + applicationID.String() + "/attach"
	body, err := json.Marshal(map[string]any{
		"user_id": userID.String(),
		"email":   email,
	})
	if err != nil {
		return err
	}
	return c.postJSON(ctx, path, body, nil)
}

func (c *HTTPDoctorApplications) getJSON(ctx context.Context, path string, out any) error {
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, req, out)
}

func (c *HTTPDoctorApplications) postJSON(ctx context.Context, path string, body []byte, out any) error {
	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(ctx, req, out)
}

func (c *HTTPDoctorApplications) doJSON(ctx context.Context, req *http.Request, out any) error {
	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("user: mesh token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	// The target is the operator-configured address of another domain on this
	// platform, never anything derived from a request, so there is no
	// user-controlled taint here for G704 to follow.
	//nolint:gosec // G704: mesh address from config, not from a request
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("user: doctor-service call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("user: read doctor-service response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrDoctorApplicationNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("user: doctor-service returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	var env envelopeData[json.RawMessage]
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("user: decode doctor-service envelope: %w", err)
	}
	if len(env.Data) == 0 {
		return json.Unmarshal(raw, out)
	}
	return json.Unmarshal(env.Data, out)
}
