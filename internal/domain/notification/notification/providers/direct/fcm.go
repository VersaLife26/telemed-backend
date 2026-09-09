// Package direct implements the default NotificationProvider set: FCM HTTP
// v1 for push, Dialog Ideamart and Twilio for SMS, and SMTP for email. None
// of these require Novu -- this is the provider ADR-002 requires to work out
// of the box.
package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"telemed/internal/domain/notification/notification"
)

// FCMConfig configures the direct Firebase Cloud Messaging HTTP v1 provider.
type FCMConfig struct {
	ProjectID string
	// ServiceAccountJSON is the raw contents of a Firebase service account
	// key file, used to mint short-lived OAuth2 access tokens for the
	// firebase.messaging scope. Never logged, never persisted by this
	// provider beyond process memory.
	ServiceAccountJSON []byte
	HTTPClient         *http.Client
}

// FCMProvider sends push notifications via https://fcm.googleapis.com/v1.
type FCMProvider struct {
	projectID string
	client    *http.Client
	tokenSrc  oauth2.TokenSource
}

// NewFCM builds an FCMProvider, parsing the service account credentials
// once at construction so a malformed key file fails fast at boot rather
// than on the first send.
func NewFCM(ctx context.Context, cfg FCMConfig) (*FCMProvider, error) {
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("direct: fcm project id is required")
	}
	if len(cfg.ServiceAccountJSON) == 0 {
		return nil, fmt.Errorf("direct: fcm service account credentials are required")
	}
	// CredentialsFromJSONWithType, not CredentialsFromJSON: the latter is
	// deprecated precisely because it accepts *any* credential configuration,
	// including an external_account config that points token minting at a
	// URL of the config author's choosing. FCM_SERVICE_ACCOUNT_JSON arrives
	// from a secret store, so pinning the type is what makes "this file was
	// swapped" a boot failure instead of an outbound-credential leak.
	creds, err := google.CredentialsFromJSONWithType(ctx, cfg.ServiceAccountJSON, google.ServiceAccount,
		"https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		return nil, fmt.Errorf("direct: parse fcm service account: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &FCMProvider{projectID: cfg.ProjectID, client: client, tokenSrc: creds.TokenSource}, nil
}

type fcmSendRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token        string            `json:"token"`
	Notification fcmNotification   `json:"notification"`
	Data         map[string]string `json:"data,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type fcmErrorResponse struct {
	Error struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

type fcmSendResponse struct {
	Name string `json:"name"`
}

// Send delivers one push message to a single device token. FCM's
// UNREGISTERED and the HTTP-404 case both mean "this token is dead" and are
// wrapped in notification.ErrTokenUnregistered so the dispatcher prunes it;
// every other 4xx is a permanent request problem, 429/5xx are transient.
func (p *FCMProvider) Send(ctx context.Context, msg notification.Message) (notification.Receipt, error) {
	if msg.DeviceToken == "" {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: fcm requires a device token"))
	}

	tok, err := p.tokenSrc.Token()
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: fcm oauth token: %w", err))
	}

	body, err := json.Marshal(fcmSendRequest{Message: fcmMessage{
		Token:        msg.DeviceToken,
		Notification: fcmNotification{Title: msg.Subject, Body: msg.Body},
		Data: map[string]string{
			"template_key":    string(msg.TemplateKey),
			"notification_id": msg.NotificationID,
		},
	}})
	if err != nil {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: fcm marshal request: %w", err))
	}

	endpoint := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: fcm build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: fcm request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode == http.StatusOK {
		var ok fcmSendResponse
		_ = json.Unmarshal(respBody, &ok)
		return notification.Receipt{ProviderMessageID: ok.Name}, nil
	}

	var fcmErr fcmErrorResponse
	_ = json.Unmarshal(respBody, &fcmErr)

	for _, d := range fcmErr.Error.Details {
		if d.ErrorCode == "UNREGISTERED" || d.ErrorCode == "NOT_FOUND" {
			return notification.Receipt{}, notification.Permanent(
				fmt.Errorf("%w: %s", notification.ErrTokenUnregistered, fcmErr.Error.Message))
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return notification.Receipt{}, notification.Permanent(
			fmt.Errorf("%w: fcm returned 404", notification.ErrTokenUnregistered))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return notification.Receipt{}, notification.Transient(
			fmt.Errorf("direct: fcm status %d: %s", resp.StatusCode, fcmErr.Error.Message))
	default:
		return notification.Receipt{}, notification.Permanent(
			fmt.Errorf("direct: fcm status %d: %s", resp.StatusCode, fcmErr.Error.Message))
	}
}

// Channels implements notification.NotificationProvider.
func (p *FCMProvider) Channels() []notification.Channel {
	return []notification.Channel{notification.ChannelPush}
}

// Name implements notification.NotificationProvider.
func (p *FCMProvider) Name() string { return "direct-fcm" }

var _ notification.NotificationProvider = (*FCMProvider)(nil)
