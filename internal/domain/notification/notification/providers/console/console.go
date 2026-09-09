// Package console implements the NotificationProvider that requires no
// external credentials at all: it prints what would have been sent. It is what
// lets `docker compose up` boot the whole platform with no FCM/Dialog/SMTP
// setup (ADR-002).
//
// It is a LOCAL DEVELOPMENT provider and nothing else. cmd/server/providers.go
// refuses to select it outside dev, and this package refuses to print message
// content unless the composition root explicitly asks it to.
package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/platform/logger"
)

// Config configures the console provider.
//
// Reveal is separated from "is this provider in use" deliberately. Whether the
// console provider is selected is a deployment decision; whether it prints a
// patient's phone number and the rendered body of their prescription
// notification to a shared file descriptor is a data-protection decision, and
// the two should not be the same switch.
type Config struct {
	Log zerolog.Logger

	// Reveal prints the recipient address, subject and rendered body verbatim.
	// Set it ONLY for local development, where seeing the message is the whole
	// point of this provider and there is no real patient behind it. Left
	// false -- the zero value, which is the safe one -- the provider prints a
	// masked recipient and the body's length instead of the body.
	Reveal bool

	// Channels this provider serves. Empty registers it for all four.
	Channels []notification.Channel
}

// Provider "sends" every message by writing it to stdout.
type Provider struct {
	log      zerolog.Logger
	reveal   bool
	channels []notification.Channel
}

// New returns a console provider.
func New(cfg Config) *Provider {
	channels := cfg.Channels
	if len(channels) == 0 {
		channels = []notification.Channel{
			notification.ChannelSMS, notification.ChannelPush,
			notification.ChannelEmail, notification.ChannelInApp,
		}
	}
	return &Provider{log: cfg.Log, reveal: cfg.Reveal, channels: channels}
}

// Send never fails and never talks to the network.
//
// SECURITY REVIEW F20(c). This used to fmt.Printf msg.To and msg.Body
// unconditionally, with a comment claiming that fmt.Printf "bypasses the
// audited log sink". It does not bypass anything that matters: in a container
// fd 1 is the same file descriptor zerolog writes to and the same stream the
// log collector ships, so the "bypass" moved the data out of the redaction
// path while leaving it in the log pipeline. msg.To is the patient's real
// phone number or email, and msg.Body is the rendered template -- for a
// prescription notification, clinical content.
//
// So the content is printed only when the composition root sets Reveal, which
// it does only for local development. Otherwise the recipient is masked and
// the body is reduced to its length, which is all a developer needs to see that
// dispatch happened.
func (p *Provider) Send(_ context.Context, msg notification.Message) (notification.Receipt, error) {
	p.log.Info().
		Str("channel", string(msg.Channel)).
		Str("template", string(msg.TemplateKey)).
		Str("locale", string(msg.Locale)).
		Str("notification_id", msg.NotificationID).
		Msg("console provider: dispatching")

	to := msg.To
	if msg.Channel == notification.ChannelPush {
		to = msg.DeviceToken
	}

	if p.reveal {
		fmt.Printf(
			"\n--- console notification (DEV, content revealed) ---------------\n"+
				"channel:  %s\ntemplate: %s\nlocale:   %s\nto:       %s\nsubject:  %s\nbody:\n%s\n"+
				"----------------------------------------------------------------\n",
			msg.Channel, msg.TemplateKey, msg.Locale, to, msg.Subject, msg.Body,
		)
	} else {
		fmt.Printf(
			"\n--- console notification (content withheld) --------------------\n"+
				"channel:  %s\ntemplate: %s\nlocale:   %s\nto:       %s\nbody:     %d characters, not printed\n"+
				"----------------------------------------------------------------\n",
			msg.Channel, msg.TemplateKey, msg.Locale, MaskRecipient(msg.Channel, to), len([]rune(msg.Body)),
		)
	}

	return notification.Receipt{ProviderMessageID: "console-" + msg.NotificationID, Delivered: true}, nil
}

// MaskRecipient renders a destination address in a form that is useful for
// correlating a support ticket and useless for identifying a patient.
//
// Exported so the masking rule has one definition and one test, rather than
// being re-derived at each call site -- which is how "no PHI in logs" turns
// into "no PHI in logs except the three places somebody forgot".
func MaskRecipient(ch notification.Channel, to string) string {
	to = strings.TrimSpace(to)
	if to == "" {
		return "(none)"
	}
	switch ch {
	case notification.ChannelEmail:
		local, domain, ok := strings.Cut(to, "@")
		if !ok {
			return logger.MaskPhone(to)
		}
		// The domain is not personal data; the local part is. Keeping the
		// domain is what makes "every failure is to one corporate domain"
		// visible in a dev console without naming anyone.
		return logger.MaskPhone(local) + "@" + domain
	case notification.ChannelPush:
		// An FCM registration token is a device-scoped bearer credential, so
		// this truncates rather than masks: enough to match against a
		// device_tokens row, not enough to send with.
		return logger.MaskID(to)
	default:
		// sms and in_app: a phone number or a user id.
		return logger.MaskPhone(to)
	}
}

// Channels implements notification.NotificationProvider.
func (p *Provider) Channels() []notification.Channel { return p.channels }

// Name implements notification.NotificationProvider.
func (p *Provider) Name() string { return "console" }

var _ notification.NotificationProvider = (*Provider)(nil)
