package main

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/notification/notification"
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
