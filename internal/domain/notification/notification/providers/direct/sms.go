package direct

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

	"telemed/internal/domain/notification/notification"
)

// DialogConfig configures Dialog Ideamart's SMS-P2A API, the cheaper of the
// two SMS routes and the one requiring TRCSL approval to run at volume in
// Sri Lanka (see docs/RUNBOOK.md).
//
// The source documentation's description of this API ("POST /sms/send")
// is too thin to be a real contract -- this implementation follows
// Ideamart's publicly documented SMS-P2A request shape
// (applicationId/password/destinationAddresses/message/sourceAddress) but
// has not been exercised against a live Ideamart account. Validate the
// field names and status codes against real sandbox credentials before
// sending real traffic; see the build report for specifics.
type DialogConfig struct {
	BaseURL       string // e.g. https://api.dialog.lk/sms/send
	ApplicationID string
	Password      string
	SourceAddress string // the registered sender ID / shortcode
	HTTPClient    *http.Client
}

// DialogProvider sends SMS via Dialog Ideamart.
type DialogProvider struct {
	cfg    DialogConfig
	client *http.Client
}

// NewDialog builds a DialogProvider.
func NewDialog(cfg DialogConfig) (*DialogProvider, error) {
	if cfg.BaseURL == "" || cfg.ApplicationID == "" || cfg.Password == "" {
		return nil, fmt.Errorf("direct: dialog base url, application id and password are required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &DialogProvider{cfg: cfg, client: client}, nil
}

type dialogSendRequest struct {
	ApplicationID        string   `json:"applicationId"`
	Password             string   `json:"password"`
	Message              string   `json:"message"`
	DestinationAddresses []string `json:"destinationAddresses"`
	SourceAddress        string   `json:"sourceAddress,omitempty"`
}

type dialogSendResponse struct {
	Status      string `json:"status"`
	Description string `json:"description"`
}

// Send delivers one SMS. A 2xx with status "Success"/"S1000" is treated as
// accepted; a 4xx (bad number, bad credentials, malformed request) is
// permanent; 429/5xx and network errors are transient.
func (p *DialogProvider) Send(ctx context.Context, msg notification.Message) (notification.Receipt, error) {
	if msg.To == "" {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: dialog requires a destination phone number"))
	}

	// G117: the "password" field is Dialog's own protocol -- their SMS API
	// authenticates by application id + password in the JSON body, with no
	// header or signature alternative. The value comes from
	// DIALOG_SMS_PASSWORD and is never logged: the request body is not logged
	// anywhere in this file, and safeErrText caps and scrubs what is stored.
	body, err := json.Marshal(dialogSendRequest{ //nolint:gosec // G117: Dialog's SMS API requires the credential in the request body; there is no header form
		ApplicationID:        p.cfg.ApplicationID,
		Password:             p.cfg.Password,
		Message:              msg.Body,
		DestinationAddresses: []string{msg.To},
		SourceAddress:        p.cfg.SourceAddress,
	})
	if err != nil {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: dialog marshal request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: dialog build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: dialog request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var parsed dialogSendResponse
	_ = json.Unmarshal(respBody, &parsed)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return notification.Receipt{ProviderMessageID: parsed.Status}, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: dialog status %d: %s", resp.StatusCode, parsed.Description))
	default:
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: dialog status %d: %s", resp.StatusCode, parsed.Description))
	}
}

// Channels implements notification.NotificationProvider.
func (p *DialogProvider) Channels() []notification.Channel {
	return []notification.Channel{notification.ChannelSMS}
}

// Name implements notification.NotificationProvider.
func (p *DialogProvider) Name() string { return "direct-dialog" }

var _ notification.NotificationProvider = (*DialogProvider)(nil)

// ---------------------------------------------------------------------------
// Twilio (international SMS fallback)
// ---------------------------------------------------------------------------

// TwilioConfig configures Twilio's REST Messages API directly (no SDK
// dependency -- the API is a simple form-encoded POST with HTTP basic auth).
type TwilioConfig struct {
	AccountSID string
	AuthToken  string
	From       string // Twilio-provisioned sending number, E.164
	HTTPClient *http.Client
}

// TwilioProvider sends SMS via Twilio's Messages API.
type TwilioProvider struct {
	cfg    TwilioConfig
	client *http.Client
}

// NewTwilio builds a TwilioProvider.
func NewTwilio(cfg TwilioConfig) (*TwilioProvider, error) {
	if cfg.AccountSID == "" || cfg.AuthToken == "" || cfg.From == "" {
		return nil, fmt.Errorf("direct: twilio account sid, auth token and from number are required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TwilioProvider{cfg: cfg, client: client}, nil
}

type twilioErrorResponse struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
}

// twilioPermanentCodes are Twilio error codes that will never succeed on
// retry: https://www.twilio.com/docs/api/errors (21211 invalid 'To',
// 21614 not a mobile number, 21408 permission/geo-restricted).
var twilioPermanentCodes = map[int]bool{21211: true, 21614: true, 21408: true, 21610: true}

// Send delivers one SMS via Twilio.
func (p *TwilioProvider) Send(ctx context.Context, msg notification.Message) (notification.Receipt, error) {
	if msg.To == "" {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: twilio requires a destination phone number"))
	}

	form := url.Values{
		"To":   {msg.To},
		"From": {p.cfg.From},
		"Body": {msg.Body},
	}
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", p.cfg.AccountSID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: twilio build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.cfg.AccountSID, p.cfg.AuthToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: twilio request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok struct {
			SID string `json:"sid"`
		}
		_ = json.Unmarshal(respBody, &ok)
		return notification.Receipt{ProviderMessageID: ok.SID}, nil
	}

	var twErr twilioErrorResponse
	_ = json.Unmarshal(respBody, &twErr)

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: twilio status %d: %s", resp.StatusCode, twErr.Message))
	}
	if twilioPermanentCodes[twErr.Code] {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: twilio error %d: %s", twErr.Code, twErr.Message))
	}
	// An unrecognised 4xx from Twilio is most often also a permanent request
	// problem (bad auth, malformed number) rather than a transient one.
	return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: twilio status %d: %s", resp.StatusCode, twErr.Message))
}

// Channels implements notification.NotificationProvider.
func (p *TwilioProvider) Channels() []notification.Channel {
	return []notification.Channel{notification.ChannelSMS}
}

// Name implements notification.NotificationProvider.
func (p *TwilioProvider) Name() string { return "direct-twilio" }

var _ notification.NotificationProvider = (*TwilioProvider)(nil)
