package notification

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/domain/notification/notification/providers/console"
	"telemed/internal/domain/notification/notification/providers/direct"
	"telemed/internal/domain/notification/notification/providers/novu"
)

// buildProviderRegistry is the composition root for NotificationProvider
// selection: it is the only place in the service that imports a concrete
// provider package (direct/novu/console), exactly as AGENT-BRIEF §0.6
// requires -- internal/notification's business logic only ever sees the
// NotificationProvider interface.
//
// Per channel, cfg picks "console" (default, no credentials, what lets
// `docker compose up` boot with nothing configured -- ADR-002), "direct"
// (FCM/Dialog-or-Twilio/SMTP, no Novu required), or "novu".
func buildProviderRegistry(ctx context.Context, cfg notification.Config, log zerolog.Logger) (*notification.Registry, error) {
	reg := notification.NewRegistry()
	b := channelBuilder{
		reg:  reg,
		log:  log,
		prod: cfg.IsProd(),
		// Content is printed by the console provider only in local
		// development. Staging is deliberately excluded along with prod: a
		// staging environment fed from a production-shaped dataset has the
		// same patients in it, and "it is only staging" is how a phone number
		// reaches a log aggregator.
		reveal: isLocalDev(cfg.Env),
	}

	// in_app has exactly one possible provider: the notification row itself.
	reg.Register(notification.NewInApp())

	var novuProvider *novu.Provider
	novuWorkflows := map[notification.Channel]string{}
	if cfg.NovuWorkflowSMS != "" {
		novuWorkflows[notification.ChannelSMS] = cfg.NovuWorkflowSMS
	}
	if cfg.NovuWorkflowPush != "" {
		novuWorkflows[notification.ChannelPush] = cfg.NovuWorkflowPush
	}
	if cfg.NovuWorkflowEmail != "" {
		novuWorkflows[notification.ChannelEmail] = cfg.NovuWorkflowEmail
	}
	needNovu := cfg.SMSProvider == "novu" || cfg.PushProvider == "novu" || cfg.EmailProvider == "novu"
	if needNovu {
		p, err := novu.New(novu.Config{BaseURL: cfg.NovuBaseURL, APIKey: cfg.NovuAPIKey, WorkflowIDs: novuWorkflows})
		if err != nil {
			return nil, fmt.Errorf("build novu provider: %w", err)
		}
		novuProvider = p
	}

	b.novu = novuProvider

	if err := b.register(notification.ChannelSMS, cfg.SMSProvider, func() (notification.NotificationProvider, error) {
		return buildDirectSMS(cfg)
	}); err != nil {
		return nil, err
	}

	if err := b.register(notification.ChannelPush, cfg.PushProvider, func() (notification.NotificationProvider, error) {
		return buildDirectFCM(ctx, cfg)
	}); err != nil {
		return nil, err
	}

	if err := b.register(notification.ChannelEmail, cfg.EmailProvider, func() (notification.NotificationProvider, error) {
		return direct.NewSMTP(direct.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
		})
	}); err != nil {
		return nil, err
	}

	return reg, nil
}

// channelBuilder carries the decisions that are the same for every channel --
// which environment this is, and therefore what the console provider is allowed
// to do -- so each register() call reads as "this channel, this selection".
type channelBuilder struct {
	reg    *notification.Registry
	log    zerolog.Logger
	novu   *novu.Provider
	prod   bool
	reveal bool
}

// register resolves one channel's provider per selection and registers it.
//
// SECURITY REVIEW F20(c). This used to fall back to the console provider for an
// unset OR MISSPELLED selection, with no environment check at all -- contrast
// user-service, which guards its equivalent. Two things follow from that in
// production, and the second is worse than the first:
//
//  1. console.Send printed the recipient's phone number and the rendered body
//     to stdout, which in a container is the same stream the log collector
//     ships. A prescription-ready notification is clinical content.
//  2. The console provider DELIVERS NOTHING. A prod deployment that lands on it
//     silently drops every appointment reminder, every OTP-adjacent message and
//     every prescription notice, while reporting each one as delivered. One
//     typo in NOTIFICATION_SMS_PROVIDER is a total, invisible outage of the
//     channel patients actually rely on.
//
// So outside local development, console is not a fallback and not a valid
// selection: boot fails, loudly, naming the variable. "Degrade to logging it
// locally" is the right instinct on a developer laptop and the wrong one on a
// telemedicine platform.
func (b channelBuilder) register(ch notification.Channel, selection string, buildDirect func() (notification.NotificationProvider, error)) error {
	switch selection {
	case "direct":
		p, err := buildDirect()
		if err != nil {
			return fmt.Errorf("build direct provider for %s: %w", ch, err)
		}
		b.reg.Register(p)
		return nil
	case "novu":
		if b.novu == nil {
			return fmt.Errorf("channel %s selects novu but no novu workflow id is configured", ch)
		}
		b.reg.Register(b.novu)
		return nil
	case "", "console":
		if b.prod {
			return fmt.Errorf(
				"channel %s resolves to the console provider (NOTIFICATION_%s_PROVIDER=%q), which delivers nothing "+
					"and prints message content: set it to \"direct\" or \"novu\" in this environment",
				ch, strings.ToUpper(string(ch)), selection)
		}
		b.reg.Register(console.New(console.Config{Log: b.log, Reveal: b.reveal, Channels: []notification.Channel{ch}}))
		return nil
	default:
		if b.prod {
			return fmt.Errorf(
				"channel %s has an unrecognised provider selection NOTIFICATION_%s_PROVIDER=%q; "+
					"expected \"direct\" or \"novu\"",
				ch, strings.ToUpper(string(ch)), selection)
		}
		b.log.Warn().Str("channel", string(ch)).Str("value", selection).
			Msg("unrecognised provider selection, falling back to console (dev only)")
		b.reg.Register(console.New(console.Config{Log: b.log, Reveal: b.reveal, Channels: []notification.Channel{ch}}))
		return nil
	}
}

// isLocalDev is deliberately narrower than config.Base.IsProd()'s inverse:
// "not prod" includes staging, and staging is not a place to print a patient's
// phone number.
func isLocalDev(env string) bool { return env == "dev" || env == "development" }

func buildDirectSMS(cfg notification.Config) (notification.NotificationProvider, error) {
	switch cfg.SMSBackend {
	case "twilio":
		return direct.NewTwilio(direct.TwilioConfig{
			AccountSID: cfg.TwilioAccountSID, AuthToken: cfg.TwilioAuthToken, From: cfg.TwilioFrom,
		})
	default:
		return direct.NewDialog(direct.DialogConfig{
			BaseURL: cfg.DialogBaseURL, ApplicationID: cfg.DialogApplicationID,
			Password: cfg.DialogPassword, SourceAddress: cfg.DialogSourceAddress,
		})
	}
}

func buildDirectFCM(ctx context.Context, cfg notification.Config) (notification.NotificationProvider, error) {
	if cfg.FCMServiceAccountJSONPath == "" {
		return nil, fmt.Errorf("FCM_SERVICE_ACCOUNT_JSON_PATH is required when NOTIFICATION_PUSH_PROVIDER=direct")
	}
	key, err := os.ReadFile(cfg.FCMServiceAccountJSONPath)
	if err != nil {
		return nil, fmt.Errorf("read fcm service account file: %w", err)
	}
	return direct.NewFCM(ctx, direct.FCMConfig{ProjectID: cfg.FCMProjectID, ServiceAccountJSON: key})
}
