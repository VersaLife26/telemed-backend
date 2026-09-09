// Package novu implements the Novu-backed NotificationProvider. Per ADR-002,
// this is an optional, config-selected alternative to the direct provider,
// not a dependency the rest of the service knows anything about -- nothing
// outside this package imports a Novu SDK or knows Novu's request shape.
//
// This service already does its own templating (see render.go and the
// seeded templates table), so the Novu workflow this provider triggers is a
// thin pass-through: it receives already-rendered subject/body in its
// payload and its job is routing, delivery analytics and Novu's dashboard,
// not template rendering. Configure one Novu workflow per channel and point
// this provider at their identifiers.
package novu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"telemed/internal/domain/notification/notification"
)

// Config configures the Novu REST API client.
type Config struct {
	BaseURL string // e.g. https://api.novu.co or a self-hosted API URL
	APIKey  string

	// WorkflowIDs maps each channel this provider instance should carry to
	// the Novu workflow identifier ("trigger name") that channel's
	// notifications are sent through. A provider built with only
	// {ChannelEmail: "..."} only ever registers itself for email.
	WorkflowIDs map[notification.Channel]string

	HTTPClient *http.Client
}

// Provider triggers Novu workflow events over Novu's REST API.
type Provider struct {
	baseURL     string
	apiKey      string
	workflowIDs map[notification.Channel]string
	channels    []notification.Channel
	client      *http.Client
}

// New builds a Novu-backed provider from cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("novu: base url is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("novu: api key is required")
	}
	if len(cfg.WorkflowIDs) == 0 {
		return nil, fmt.Errorf("novu: at least one channel workflow id is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	channels := make([]notification.Channel, 0, len(cfg.WorkflowIDs))
	for ch := range cfg.WorkflowIDs {
		channels = append(channels, ch)
	}
	return &Provider{
		baseURL: cfg.BaseURL, apiKey: cfg.APIKey,
		workflowIDs: cfg.WorkflowIDs, channels: channels, client: client,
	}, nil
}

type triggerSubscriber struct {
	SubscriberID string `json:"subscriberId"`
	Phone        string `json:"phone,omitempty"`
	Email        string `json:"email,omitempty"`
}

type triggerRequest struct {
	Name    string            `json:"name"`
	To      triggerSubscriber `json:"to"`
	Payload triggerPayload    `json:"payload"`
}

type triggerPayload struct {
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	TemplateKey string `json:"templateKey"`
	Locale      string `json:"locale"`
}

type triggerResponse struct {
	Data struct {
		Acknowledged  bool   `json:"acknowledged"`
		Status        string `json:"status"`
		TransactionID string `json:"transactionId"`
	} `json:"data"`
}

type triggerErrorResponse struct {
	Message []string `json:"message"`
	Error   string   `json:"error"`
}

// Send triggers the Novu workflow configured for msg.Channel.
func (p *Provider) Send(ctx context.Context, msg notification.Message) (notification.Receipt, error) {
	workflow, ok := p.workflowIDs[msg.Channel]
	if !ok {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("novu: no workflow configured for channel %s", msg.Channel))
	}

	subscriber := triggerSubscriber{SubscriberID: msg.NotificationID}
	switch msg.Channel {
	case notification.ChannelSMS:
		subscriber.Phone = msg.To
	case notification.ChannelEmail:
		subscriber.Email = msg.To
	case notification.ChannelPush:
		// Novu resolves push tokens against subscriber credentials it
		// manages itself; this provider does not hand it a raw device
		// token per call. Operators running push through Novu register
		// tokens with Novu directly (out of band from this service).
	}

	body, err := json.Marshal(triggerRequest{
		Name: workflow,
		To:   subscriber,
		Payload: triggerPayload{
			Subject: msg.Subject, Body: msg.Body,
			TemplateKey: string(msg.TemplateKey), Locale: string(msg.Locale),
		},
	})
	if err != nil {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("novu: marshal trigger request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/events/trigger", bytes.NewReader(body))
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("novu: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "ApiKey "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("novu: request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok triggerResponse
		_ = json.Unmarshal(respBody, &ok)
		return notification.Receipt{ProviderMessageID: ok.Data.TransactionID}, nil
	}

	var errResp triggerErrorResponse
	_ = json.Unmarshal(respBody, &errResp)

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return notification.Receipt{}, notification.Transient(fmt.Errorf("novu: status %d: %v", resp.StatusCode, errResp.Message))
	}
	return notification.Receipt{}, notification.Permanent(fmt.Errorf("novu: status %d: %v", resp.StatusCode, errResp.Message))
}

// Channels implements notification.NotificationProvider.
func (p *Provider) Channels() []notification.Channel { return p.channels }

// Name implements notification.NotificationProvider.
func (p *Provider) Name() string { return "novu" }

var _ notification.NotificationProvider = (*Provider)(nil)
