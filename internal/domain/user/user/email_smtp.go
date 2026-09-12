package user

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"time"
)

// SMTPEmailConfig configures the OTP email transport. Plain SMTP with
// STARTTLS, which net/smtp.SendMail negotiates automatically when the server
// advertises it -- the same shape Gmail, SES and any relay expose, so no
// vendor SDK is needed.
type SMTPEmailConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	Timeout  time.Duration
}

// SMTPEmailSender delivers OTP mail over SMTP.
type SMTPEmailSender struct {
	cfg SMTPEmailConfig
}

var _ EmailSender = (*SMTPEmailSender)(nil)

// NewSMTPEmailSender builds the transport. It fails rather than degrading:
// an OTP sender that cannot send is a locked front door, and finding that out
// at boot is much cheaper than finding it out from a patient who cannot log in.
func NewSMTPEmailSender(cfg SMTPEmailConfig) (*SMTPEmailSender, error) {
	if cfg.Host == "" || cfg.Port == 0 {
		return nil, fmt.Errorf("user: SMTP_HOST and SMTP_PORT are required to send one-time codes")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("user: SMTP_FROM is required to send one-time codes")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SMTPEmailSender{cfg: cfg}, nil
}

// Send delivers one plain-text message.
//
// Headers are rejected rather than sanitised if the recipient or subject
// carries a newline: both are interpolated into the header block, so a "\r\n"
// in either would let a caller inject headers -- a Bcc to an attacker on the
// one message that carries a login code. The recipient here comes from an
// account record or a validated request field, so this should never fire; it
// is the check that keeps it that way.
func (s *SMTPEmailSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("user: smtp send needs a recipient")
	}
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return "", fmt.Errorf("user: smtp recipient and subject may not contain line breaks")
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}

	raw := strings.Join([]string{
		"From: " + s.cfg.From,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		`Content-Type: text/plain; charset="UTF-8"`,
	}, "\r\n") + "\r\n\r\n" + body

	// net/smtp has no context support, so the call runs on its own goroutine
	// and the caller's deadline is honoured here. A send that outlives the
	// request keeps running to completion rather than being abandoned
	// half-way through an SMTP conversation.
	done := make(chan error, 1)
	go func() {
		// Recipient and subject were rejected above if they contain line breaks,
		// which is the SMTP header-injection vector gosec G707 flags here.
		done <- smtp.SendMail(addr, auth, s.cfg.From, []string{to}, []byte(raw)) //nolint:gosec // G707: CRLF rejected on to/subject above
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("user: smtp send: %w", err)
		}
		return "", nil
	case <-time.After(s.cfg.Timeout):
		return "", fmt.Errorf("user: smtp send timed out after %s", s.cfg.Timeout)
	}
}
