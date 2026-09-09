package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"telemed/internal/domain/notification/notification"
)

// SMTPConfig configures the direct email provider. It speaks plain SMTP
// with STARTTLS (net/smtp.SendMail negotiates STARTTLS automatically when
// the server advertises it), which is also exactly the interface AWS SES
// exposes -- so this one provider covers both "real SMTP relay" and SES
// without a dedicated SES SDK dependency.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	Timeout  time.Duration
}

// SMTPProvider sends email via net/smtp.
type SMTPProvider struct {
	cfg SMTPConfig
}

// NewSMTP builds an SMTPProvider.
func NewSMTP(cfg SMTPConfig) (*SMTPProvider, error) {
	if cfg.Host == "" || cfg.Port == 0 {
		return nil, fmt.Errorf("direct: smtp host and port are required")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("direct: smtp from address is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SMTPProvider{cfg: cfg}, nil
}

// Send delivers one HTML email. msg.Body is already html/template-escaped
// by the render layer -- this provider treats it as an opaque HTML blob and
// never re-parses or re-escapes it.
func (p *SMTPProvider) Send(ctx context.Context, msg notification.Message) (notification.Receipt, error) {
	if msg.To == "" {
		return notification.Receipt{}, notification.Permanent(fmt.Errorf("direct: smtp requires a recipient email address"))
	}

	addr := fmt.Sprintf("%s:%d", p.cfg.Host, p.cfg.Port)
	var auth smtp.Auth
	if p.cfg.Username != "" {
		auth = smtp.PlainAuth("", p.cfg.Username, p.cfg.Password, p.cfg.Host)
	}

	headerLines := []string{
		"From: " + p.cfg.From,
		"To: " + msg.To,
		"Subject: " + msg.Subject,
		"MIME-Version: 1.0",
		`Content-Type: text/html; charset="UTF-8"`,
	}
	raw := strings.Join(headerLines, "\r\n") + "\r\n\r\n" + msg.Body

	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(addr, auth, p.cfg.From, []string{msg.To}, []byte(raw))
	}()

	timeout := p.cfg.Timeout
	select {
	case <-ctx.Done():
		return notification.Receipt{}, notification.Transient(ctx.Err())
	case err := <-done:
		if err == nil {
			return notification.Receipt{}, nil
		}
		return notification.Receipt{}, classifySMTPError(err)
	case <-time.After(timeout):
		return notification.Receipt{}, notification.Transient(fmt.Errorf("direct: smtp send timed out after %s", timeout))
	}
}

// classifySMTPError distinguishes a permanent rejection (bad mailbox, "550
// no such user") from a transient one (connection reset, greylisting, 4xx
// temporary failure).
func classifySMTPError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return notification.Transient(err)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "550"), strings.Contains(msg, "no such user"),
		strings.Contains(msg, "mailbox unavailable"), strings.Contains(msg, "user unknown"),
		strings.Contains(msg, "invalid address"):
		return notification.Permanent(err)
	case strings.Contains(msg, "421"), strings.Contains(msg, "450"),
		strings.Contains(msg, "451"), strings.Contains(msg, "452"), strings.Contains(msg, "greylist"):
		return notification.Transient(err)
	default:
		// An SMTP error we do not recognise: default transient, matching
		// Classify's own default -- a wrongly-retried permanent failure
		// costs a few attempts, a wrongly-abandoned transient one costs a
		// notification.
		return notification.Transient(err)
	}
}

// Channels implements notification.NotificationProvider.
func (p *SMTPProvider) Channels() []notification.Channel {
	return []notification.Channel{notification.ChannelEmail}
}

// Name implements notification.NotificationProvider.
func (p *SMTPProvider) Name() string { return "direct-smtp" }

var _ notification.NotificationProvider = (*SMTPProvider)(nil)
