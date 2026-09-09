package user

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"
)

// httpDoer is the narrow slice of *http.Client every provider needs, so tests
// can substitute a fake transport without standing up a server.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// -----------------------------------------------------------------------
// Dev provider: prints the OTP to the console. This is the default when no
// SMS credentials are configured, which is what lets a new hire run the
// service locally with zero external accounts.
// -----------------------------------------------------------------------

// DevSMSProvider logs messages instead of sending them. Never select this in
// a prod environment -- main.go refuses to unless ENV=dev.
type DevSMSProvider struct {
	log zerolog.Logger
}

// NewDevSMSProvider builds the console-logging provider.
func NewDevSMSProvider(log zerolog.Logger) *DevSMSProvider {
	return &DevSMSProvider{log: log}
}

func (p *DevSMSProvider) Send(_ context.Context, phone, body string) (string, error) {
	// This is the one place an OTP is intentionally allowed on stdout: dev
	// mode only, and it is why no SMS account is needed to develop locally.
	p.log.Warn().
		Str("phone", phone).
		Str("body", body).
		Msg("[DEV SMS] no SMS provider configured -- printing message instead of sending")
	fmt.Printf("\n=== DEV SMS to %s ===\n%s\n======================\n\n", phone, body)
	return "dev-" + phone, nil
}

// -----------------------------------------------------------------------
// Dialog Ideamart -- the primary carrier billing / SMS gateway for Sri Lanka.
// -----------------------------------------------------------------------

// DialogConfig configures the Dialog Ideamart SMS API client.
type DialogConfig struct {
	BaseURL       string // e.g. https://api.dialog.lk/sms
	ApplicationID string
	Password      string
	SourceAddress string // the sender id/short code Dialog assigned
}

// DialogSMSProvider sends SMS via the Dialog Ideamart HTTP API.
//
// We call the REST endpoint directly with net/http rather than pull in a
// vendor SDK: Ideamart's Go SDK is unmaintained and the API surface we need
// is one POST with a JSON body, which is not worth a dependency.
type DialogSMSProvider struct {
	cfg    DialogConfig
	client httpDoer
}

// NewDialogSMSProvider builds the Dialog-backed provider.
func NewDialogSMSProvider(cfg DialogConfig) *DialogSMSProvider {
	return &DialogSMSProvider{cfg: cfg, client: newHTTPClient()}
}

type dialogSendRequest struct {
	ApplicationID string `json:"applicationId"`
	Password      string `json:"password"`
	Message       string `json:"message"`
	Destination   string `json:"destinationAddresses"`
	SourceAddress string `json:"sourceAddress"`
}

type dialogSendResponse struct {
	StatusCode    string `json:"statusCode"`
	StatusDetail  string `json:"statusDetail"`
	TransactionID string `json:"transactionId"`
}

func (p *DialogSMSProvider) Send(ctx context.Context, phone, body string) (string, error) {
	payload := dialogSendRequest{
		ApplicationID: p.cfg.ApplicationID,
		Password:      p.cfg.Password,
		Message:       body,
		Destination:   phone,
		SourceAddress: p.cfg.SourceAddress,
	}
	// G117 flags the `Password` field as a secret being marshalled. It is,
	// and that is the Ideamart contract: Dialog authenticates the send by
	// applicationId + password in the JSON body, with no header alternative.
	// What matters for a secret is where it GOES -- this buffer is written
	// straight to the request body over TLS to Dialog and is never logged,
	// never stored, and never returned. The failure paths below quote
	// resp.Body, never `raw`.
	raw, err := json.Marshal(payload) //nolint:gosec // G117: the password is Dialog's documented body-auth parameter; the marshalled bytes go only into the outbound request body, never to a log or a store.
	if err != nil {
		return "", fmt.Errorf("user: marshal dialog request: %w", err)
	}

	// G704 (SSRF) is a false positive: endpoint is DIALOG_BASE_URL, a
	// deployment setting read once at boot from the process environment, plus
	// a constant path. No request data reaches it -- `phone` and `body` travel
	// in the JSON payload, not the URL -- so there is no taint from a caller
	// to the destination host. An operator who can set DIALOG_BASE_URL already
	// chooses which SMS gateway this service talks to.
	endpoint := p.cfg.BaseURL + "/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw)) //nolint:gosec // G704: endpoint is built from boot-time config (DIALOG_BASE_URL) and a constant path; see the comment above.
	if err != nil {
		return "", fmt.Errorf("user: build dialog request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("user: dialog send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body2, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("user: read dialog response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("user: dialog send failed: status %d: %s", resp.StatusCode, string(body2))
	}

	var out dialogSendResponse
	if err := json.Unmarshal(body2, &out); err != nil {
		// Some Dialog deployments return plain text rather than JSON on
		// success; treat a 2xx as success even when the body does not parse.
		return "dialog-unparsed", nil //nolint:nilerr
	}
	return out.TransactionID, nil
}

// -----------------------------------------------------------------------
// Twilio -- used for international numbers and as a documented fallback.
// -----------------------------------------------------------------------

// TwilioConfig configures the Twilio Programmable Messaging API client.
type TwilioConfig struct {
	AccountSID string
	AuthToken  string
	FromNumber string
	BaseURL    string // defaults to https://api.twilio.com
}

// TwilioSMSProvider sends SMS via Twilio's REST API. Implemented against the
// plain HTTP API (basic auth + form body) rather than the twilio-go SDK: the
// SDK pulls a large dependency tree for one endpoint, and AGENT-BRIEF's
// canonical dependency list does not pin a Twilio SDK version.
type TwilioSMSProvider struct {
	cfg    TwilioConfig
	client httpDoer
}

// NewTwilioSMSProvider builds the Twilio-backed provider.
func NewTwilioSMSProvider(cfg TwilioConfig) *TwilioSMSProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.twilio.com"
	}
	return &TwilioSMSProvider{cfg: cfg, client: newHTTPClient()}
}

type twilioResponse struct {
	SID     string `json:"sid"`
	Status  string `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (p *TwilioSMSProvider) Send(ctx context.Context, phone, body string) (string, error) {
	// G704 (SSRF) is a false positive for the same reason as the Dialog
	// provider above: both interpolated values are boot-time config
	// (TWILIO_BASE_URL, TWILIO_ACCOUNT_SID), and the destination phone number
	// goes in the form body, not the URL.
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", p.cfg.BaseURL, p.cfg.AccountSID)

	form := url.Values{}
	form.Set("To", phone)
	form.Set("From", p.cfg.FromNumber)
	form.Set("Body", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode())) //nolint:gosec // G704: endpoint is built from boot-time config (TWILIO_BASE_URL, TWILIO_ACCOUNT_SID); see the comment above.
	if err != nil {
		return "", fmt.Errorf("user: build twilio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	auth := base64.StdEncoding.EncodeToString([]byte(p.cfg.AccountSID + ":" + p.cfg.AuthToken))
	req.Header.Set("Authorization", "Basic "+auth)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("user: twilio send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("user: read twilio response: %w", err)
	}

	var out twilioResponse
	_ = json.Unmarshal(raw, &out)

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("user: twilio send failed: status %d: %s", resp.StatusCode, out.Message)
	}
	return out.SID, nil
}
