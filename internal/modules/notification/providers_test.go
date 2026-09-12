package notification

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/domain/notification/notification/providers/direct"
)

func builderFor(env string) channelBuilder {
	return channelBuilder{
		reg:    notification.NewRegistry(),
		log:    zerolog.Nop(),
		prod:   env == "prod" || env == "production",
		reveal: isLocalDev(env),
	}
}

// buildDirectNever is handed to register for cases that must never reach the
// "direct" branch. If one does, the test fails with a clear reason instead of
// a confusing nil.
func buildDirectNever(t *testing.T) func() (notification.NotificationProvider, error) {
	return func() (notification.NotificationProvider, error) {
		t.Fatal("register must not have taken the direct branch for this selection")
		return nil, nil
	}
}

// TestRegister_ProdRefusesTheConsoleProvider is security review F20(c).
//
// registerChannel fell back to console for an unset or misspelled selection,
// with no IsProd check anywhere. Two consequences in production, and the second
// is the one that would have hurt first:
//
//   - console.Send printed the recipient's phone number and the rendered body.
//   - the console provider delivers nothing at all, while reporting every
//     message as delivered. One typo in NOTIFICATION_SMS_PROVIDER is a silent,
//     total outage of the channel patients rely on for appointment reminders.
//
// Delete the `if b.prod` guards from register and every case below returns nil
// and boots happily into that state.
func TestRegister_ProdRefusesTheConsoleProvider(t *testing.T) {
	for _, env := range []string{"prod", "production"} {
		for _, selection := range []string{"", "console", "consoel", "Direct", "novu-typo"} {
			t.Run(env+"/"+selection, func(t *testing.T) {
				err := builderFor(env).register(notification.ChannelSMS, selection, buildDirectNever(t))
				require.Error(t, err,
					"selection %q must not be accepted in %s: it resolves to a provider that delivers nothing",
					selection, env)
				require.Contains(t, err.Error(), "NOTIFICATION_SMS_PROVIDER")
			})
		}
	}
}

// TestRegister_DevStillFallsBackToConsole. The fallback is what lets
// `docker compose up` boot the platform with no FCM/Dialog/SMTP credentials
// (ADR-002), and that has to keep working or the guard above gets reverted by
// the first person who wants to run the stack locally.
func TestRegister_DevStillFallsBackToConsole(t *testing.T) {
	for _, env := range []string{"dev", "development", "staging"} {
		for _, selection := range []string{"", "console", "typo"} {
			t.Run(env+"/"+selection, func(t *testing.T) {
				b := builderFor(env)
				require.NoError(t, b.register(notification.ChannelSMS, selection, buildDirectNever(t)))

				p, ok := b.reg.ProviderFor(notification.ChannelSMS)
				require.True(t, ok, "a provider must be registered for sms")
				require.Equal(t, "console", p.Name())
			})
		}
	}
}

// TestReveal_IsDevOnly. "Not prod" is not the same question as "is this a
// developer's laptop". A staging environment seeded from a production-shaped
// dataset holds the same phone numbers, so the console provider must withhold
// content there even though it is allowed to run.
func TestReveal_IsDevOnly(t *testing.T) {
	require.True(t, isLocalDev("dev"))
	require.True(t, isLocalDev("development"))
	require.False(t, isLocalDev("staging"))
	require.False(t, isLocalDev("prod"))
	require.False(t, isLocalDev("production"))
	require.False(t, isLocalDev(""), "an unset ENV must not reveal message content")

	require.False(t, builderFor("staging").reveal,
		"staging registers the console provider but must not let it print message bodies")
	require.True(t, builderFor("dev").reveal)
}

// TestRegister_NovuWithoutAWorkflowIsStillAnError guards the one pre-existing
// hard failure in this function, which the restructure must not have dropped.
func TestRegister_NovuWithoutAWorkflowIsStillAnError(t *testing.T) {
	err := builderFor("dev").register(notification.ChannelEmail, "novu", buildDirectNever(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "novu")
}

// TestRegister_DisabledIsExplicitAndFailsLoudly. "disabled" is the honest way
// to express a channel whose credentials do not exist -- unlike console, which
// reports every send successful and prints the recipient and body to stdout.
// It must be reachable in production (that is the whole point) while an unset
// or misspelled value still refuses to boot.
func TestRegister_DisabledIsExplicitAndFailsLoudly(t *testing.T) {
	for _, env := range []string{"prod", "production", "dev"} {
		t.Run(env, func(t *testing.T) {
			b := builderFor(env)
			require.NoError(t, b.register(notification.ChannelPush, "disabled", buildDirectNever(t)))

			p, ok := b.reg.ProviderFor(notification.ChannelPush)
			require.True(t, ok, "a provider must be registered for push")
			require.Equal(t, "unavailable", p.Name())

			// Every send fails permanently, and nothing is reported delivered.
			receipt, err := p.Send(context.Background(), notification.Message{To: "device-token"})
			require.Error(t, err)
			require.False(t, receipt.Delivered, "a disabled channel must never report a delivery")
			require.Contains(t, err.Error(), "NOTIFICATION_PUSH_PROVIDER=disabled")
		})
	}
}

// TestRegister_EmailTransportIsSMSOnly. Routing a channel's messages to email
// is a stopgap for the missing SMS rail. The email channel already is email,
// and push has no equivalent, so either selecting it is a config mistake that
// must not boot.
func TestRegister_EmailTransportIsSMSOnly(t *testing.T) {
	for _, ch := range []notification.Channel{notification.ChannelPush, notification.ChannelEmail} {
		t.Run(string(ch), func(t *testing.T) {
			b := builderFor("production")
			b.buildSMTP = func() (notification.NotificationProvider, error) {
				t.Fatal("the smtp transport must not be built for a channel that may not select it")
				return nil, nil
			}
			err := b.register(ch, "email", buildDirectNever(t))
			require.Error(t, err)
			require.Contains(t, err.Error(), "sms-only")
		})
	}
}

// TestRegister_SMSOverEmailAnswersForTheSMSChannel. The registry dispatches on
// Channels(), so an SMTP provider registered for sms must claim sms -- not
// email, which would leave the sms channel resolving to nothing while the
// config said it was handled.
func TestRegister_SMSOverEmailAnswersForTheSMSChannel(t *testing.T) {
	b := builderFor("production")
	b.buildSMTP = func() (notification.NotificationProvider, error) {
		return direct.NewSMTP(direct.SMTPConfig{Host: "smtp.example.test", Port: 587, From: "noreply@example.test"})
	}

	require.NoError(t, b.register(notification.ChannelSMS, "email", buildDirectNever(t)))

	p, ok := b.reg.ProviderFor(notification.ChannelSMS)
	require.True(t, ok, "the sms channel must resolve to the email transport")
	require.Equal(t, []notification.Channel{notification.ChannelSMS}, p.Channels())
}
