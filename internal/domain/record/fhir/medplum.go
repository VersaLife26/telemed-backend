package fhir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MedplumConfig configures the REST client against a self-hosted or hosted
// Medplum FHIR R4 server (SDD §19: "FHIR Server: Medplum ... Go client: REST").
type MedplumConfig struct {
	// BaseURL is the server root, e.g. "https://api.medplum.com" or
	// "http://localhost:8103". The client appends "/fhir/R4" for resource
	// calls and "/oauth2/token" for authentication.
	BaseURL      string
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client
}

// MedplumClient is the Client implementation backed by Medplum's FHIR R4 REST
// API, authenticating via OAuth2 client-credentials.
type MedplumClient struct {
	baseURL      string
	clientID     string
	clientSecret string
	http         *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

var _ Client = (*MedplumClient)(nil)

// NewMedplum builds a client. It performs no network I/O until the first
// call, so a misconfigured or unreachable Medplum instance does not prevent
// the service from starting -- it surfaces as an error on the first FHIR
// write, which the caller logs and, for non-critical paths, tolerates.
func NewMedplum(cfg MedplumConfig) *MedplumClient {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &MedplumClient{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		http:         httpClient,
	}
}

// token returns a cached bearer token, refreshing it once it is within 30
// seconds of expiry.
func (m *MedplumClient) token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && time.Now().Add(30*time.Second).Before(m.expiresAt) {
		return m.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", m.clientID)
	form.Set("client_secret", m.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("fhir: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fhir: request token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("fhir: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fhir: token request failed: %d (%s)", resp.StatusCode, summariseFHIRError(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("fhir: decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("fhir: token response missing access_token")
	}

	m.accessToken = tok.AccessToken
	if tok.ExpiresIn > 0 {
		m.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		m.expiresAt = time.Now().Add(5 * time.Minute)
	}
	return m.accessToken, nil
}

// post executes one FHIR REST write and decodes the JSON response into out.
//
// POST is the only verb this client uses, and that is a property of what the
// service does rather than a limitation: every resource it owns is created,
// never updated in place. A prescription is immutable once issued, and each
// revision of a clinical note is its own Composition linked to the one it
// replaces -- so there is nothing here for a PUT to target. extraHeaders
// carries conditional-create's If-None-Exist, which is how the Patient /
// Practitioner / Encounter upserts avoid duplicating a resource that already
// exists.
//
// Every caller only cares whether the call succeeded, so this returns just an
// error; a status code can be added back if a future caller needs to branch
// on 200-vs-201 (conditional create's "matched an existing resource" vs
// "created a new one").
func (m *MedplumClient) post(ctx context.Context, resourcePath string, body any, extraHeaders map[string]string, out any) error {
	tok, err := m.token(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fhir: marshal request body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/fhir/R4/"+resourcePath, reader)
	if err != nil {
		return fmt.Errorf("fhir: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set("Accept", "application/fhir+json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("fhir: POST %s: %w", resourcePath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("fhir: read response body: %w", err)
	}

	if resp.StatusCode >= 300 {
		return fmt.Errorf("fhir: POST %s failed: %d (%s)", resourcePath, resp.StatusCode, summariseFHIRError(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("fhir: decode %s response: %w", resourcePath, err)
		}
	}
	return nil
}

// maxSummarisedIssues bounds how much of an OperationOutcome reaches the log
// line, so a server that returns one issue per submitted field cannot turn a
// single failure into an unbounded log record.
const maxSummarisedIssues = 5

// summariseFHIRError renders a non-2xx FHIR response body as something safe
// to put in an error string.
//
// The body used to be interpolated whole. That is a PHI leak with a FHIR
// server on the far end of it: a rejected MedicationRequest routinely echoes
// the medication text back in OperationOutcome.issue[].diagnostics or
// .details.text, and a rejected DocumentReference echoes the attachment
// title -- which on this platform is the patient's own filename. Those
// errors are wrapped and logged by the callers in records and prescriptions
// alongside an unmasked document_id or prescription_id, which makes the
// content directly patient-linkable (SECURITY-REVIEW F20b).
//
// What survives is only what comes from a closed FHIR value set: issue
// severity and issue code. Those name the CLASS of failure ("error",
// "invalid", "duplicate", "security"), which is what an operator debugging a
// FHIR integration actually needs, and they cannot carry a drug name, a
// diagnosis or a filename because a server may not put one there. Free-text
// diagnostics and details.text are dropped, not truncated: half of a
// medication name is still a medication name.
//
// A body that is not a parseable OperationOutcome is described rather than
// quoted, because an unrecognised shape is exactly the one whose contents
// cannot be reasoned about.
func summariseFHIRError(raw []byte) string {
	if len(raw) == 0 {
		return "empty body"
	}

	var oo struct {
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Severity string `json:"severity"`
			Code     string `json:"code"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(raw, &oo); err != nil || oo.ResourceType != "OperationOutcome" || len(oo.Issue) == 0 {
		return fmt.Sprintf("body suppressed, %d bytes", len(raw))
	}

	issues := oo.Issue
	truncated := false
	if len(issues) > maxSummarisedIssues {
		issues, truncated = issues[:maxSummarisedIssues], true
	}
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, fhirToken(i.Severity)+"/"+fhirToken(i.Code))
	}
	out := "OperationOutcome " + strings.Join(parts, ", ")
	if truncated {
		out += fmt.Sprintf(", +%d more", len(oo.Issue)-maxSummarisedIssues)
	}
	return out
}

// fhirToken keeps a value only if it looks like a FHIR code -- lowercase
// letters, digits and dashes, short. A server that puts free text where a
// code belongs gets its free text dropped rather than trusted because of
// where it appeared in the JSON.
func fhirToken(v string) string {
	if v == "" || len(v) > 40 {
		return "?"
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "?"
		}
	}
	return v
}

// upsertByIdentifier performs a FHIR conditional create: the server creates
// the resource unless one already carries the given identifier, in which
// case it returns the existing resource. This is the standard FHIR idiom for
// "create or reuse the corresponding resource in the FHIR store", and it
// keeps a duplicate consultation.ended redelivery from producing two
// Encounter resources for the same appointment.
func upsertByIdentifier[T any](ctx context.Context, m *MedplumClient, resourceType, system, value string, body T) (string, error) {
	headers := map[string]string{
		"If-None-Exist": fmt.Sprintf("identifier=%s|%s", url.QueryEscape(system), url.QueryEscape(value)),
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := m.post(ctx, resourceType, body, headers, &result); err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", fmt.Errorf("fhir: %s upsert returned no id", resourceType)
	}
	return result.ID, nil
}

func (m *MedplumClient) UpsertPatient(ctx context.Context, p Patient) (string, error) {
	p.ResourceType = "Patient"
	system, value := identifierOf(p.Identifier)
	return upsertByIdentifier(ctx, m, "Patient", system, value, p)
}

func (m *MedplumClient) UpsertPractitioner(ctx context.Context, p Practitioner) (string, error) {
	p.ResourceType = "Practitioner"
	system, value := identifierOf(p.Identifier)
	return upsertByIdentifier(ctx, m, "Practitioner", system, value, p)
}

func (m *MedplumClient) UpsertEncounter(ctx context.Context, e Encounter) (string, error) {
	e.ResourceType = "Encounter"
	system, value := identifierOf(e.Identifier)
	return upsertByIdentifier(ctx, m, "Encounter", system, value, e)
}

func (m *MedplumClient) CreateMedicationRequest(ctx context.Context, mr MedicationRequest) (string, error) {
	mr.ResourceType = "MedicationRequest"
	var result struct {
		ID string `json:"id"`
	}
	if err := m.post(ctx, "MedicationRequest", mr, nil, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (m *MedplumClient) CreateDocumentReference(ctx context.Context, dr DocumentReference) (string, error) {
	dr.ResourceType = "DocumentReference"
	var result struct {
		ID string `json:"id"`
	}
	if err := m.post(ctx, "DocumentReference", dr, nil, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (m *MedplumClient) CreateComposition(ctx context.Context, c Composition) (string, error) {
	c.ResourceType = "Composition"
	var result struct {
		ID string `json:"id"`
	}
	if err := m.post(ctx, "Composition", c, nil, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (m *MedplumClient) CreateCondition(ctx context.Context, c Condition) (string, error) {
	c.ResourceType = "Condition"
	var result struct {
		ID string `json:"id"`
	}
	if err := m.post(ctx, "Condition", c, nil, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func identifierOf(ids []Identifier) (system, value string) {
	if len(ids) == 0 {
		return "", ""
	}
	return ids[0].System, ids[0].Value
}
