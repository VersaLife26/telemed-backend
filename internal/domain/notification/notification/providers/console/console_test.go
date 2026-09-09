package console_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/domain/notification/notification/providers/console"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written to it.
//
// The redirection is the point of the test, not an implementation detail. The
// console provider writes with fmt.Printf specifically to sidestep the
// structured logger, and its old comment claimed that "bypasses the audited
// log sink" -- so asserting against a zerolog buffer would test the wrong
// stream entirely and pass while the phone number went to fd 1 regardless.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()

	require.NoError(t, w.Close())
	os.Stdout = orig
	out := <-done
	require.NoError(t, r.Close())
	return out
}

func prescriptionMessage() notification.Message {
	return notification.Message{
		Channel:        notification.ChannelSMS,
		TemplateKey:    "prescription_ready",
		Locale:         "en",
		NotificationID: "11111111-2222-3333-4444-555555555555",
		To:             "+94771234567",
		Subject:        "Your prescription is ready",
		Body:           "Your prescription from Dr. Ruwan Fernando is ready to download.",
	}
}

// TestConsole_WithholdsRecipientAndBodyByDefault is security review F20(c).
//
// console.Send used to fmt.Printf msg.To and msg.Body unconditionally. msg.To
// is the patient's real phone number; msg.Body is the rendered template, which
// for a prescription notification names the prescribing doctor and, in other
// templates, the clinical event. In a container fd 1 is the same stream zerolog
// writes to and the same one the log collector ships, so "it bypasses the log
// sink" bought nothing except skipping redaction.
//
// Take the `if p.reveal` branch out of console.Send and this test fails: the
// number and the body are back on stdout.
func TestConsole_WithholdsRecipientAndBodyByDefault(t *testing.T) {
	p := console.New(console.Config{Log: zerolog.Nop(), Channels: []notification.Channel{notification.ChannelSMS}})
	msg := prescriptionMessage()

	out := captureStdout(t, func() {
		_, err := p.Send(context.Background(), msg)
		require.NoError(t, err)
	})

	require.NotContains(t, out, msg.To, "the recipient's phone number must not reach stdout")
	require.NotContains(t, out, "771234567", "not even the subscriber part of it")
	require.NotContains(t, out, msg.Body, "the rendered body must not reach stdout")
	require.NotContains(t, out, "Ruwan Fernando", "nor any fragment of it")

	// It still has to be useful: which channel, which template, and enough of
	// the recipient to match a support ticket against.
	require.Contains(t, out, "prescription_ready")
	require.Contains(t, out, "4567", "the last four digits are kept for correlation")
	require.Contains(t, out, "not printed")
}

// TestConsole_RevealsOnlyWhenAskedTo. The provider is still what makes
// `docker compose up` usable with nothing configured, and a developer
// exercising it locally needs to see the message. That is a separate decision
// from "this provider is selected", and cmd/server/providers.go makes it only
// for dev and development -- never staging, never prod.
func TestConsole_RevealsOnlyWhenAskedTo(t *testing.T) {
	p := console.New(console.Config{Log: zerolog.Nop(), Reveal: true})
	msg := prescriptionMessage()

	out := captureStdout(t, func() {
		_, err := p.Send(context.Background(), msg)
		require.NoError(t, err)
	})

	require.Contains(t, out, msg.To)
	require.Contains(t, out, msg.Body)
	require.Contains(t, out, "DEV")
}

// TestConsole_PushPrintsTheDeviceTokenMasked. For push, the destination is an
// FCM registration token -- a device-scoped bearer credential. Printing it in
// full is a different problem from printing a phone number and an equally real
// one.
func TestConsole_PushPrintsTheDeviceTokenMasked(t *testing.T) {
	token := "fcm-registration-token-abcdefghijklmnop"
	p := console.New(console.Config{Log: zerolog.Nop()})

	out := captureStdout(t, func() {
		_, err := p.Send(context.Background(), notification.Message{
			Channel: notification.ChannelPush, TemplateKey: "reminder_1h",
			NotificationID: "n1", DeviceToken: token, Body: "Your consultation starts in an hour.",
		})
		require.NoError(t, err)
	})

	require.NotContains(t, out, token)
	require.Contains(t, out, "fcm-regi...")
}

func TestMaskRecipient(t *testing.T) {
	cases := []struct {
		name string
		ch   notification.Channel
		in   string
		want string
	}{
		{"phone keeps the last four", notification.ChannelSMS, "+94771234567", "********4567"},
		{"email keeps the domain only", notification.ChannelEmail, "kamal.perera@example.lk", "********rera@example.lk"},
		{"malformed email is treated as an opaque identifier", notification.ChannelEmail, "not-an-email", "********mail"},
		{"push token is truncated", notification.ChannelPush, "abcdefghijklmnop", "abcdefgh..."},
		{"in_app user id", notification.ChannelInApp, "8f14e45f-ea8b-4c1e-9f3a-7d2b6c5a1e90", "********************************1e90"},
		{"empty", notification.ChannelSMS, "", "(none)"},
		{"whitespace only", notification.ChannelSMS, "   ", "(none)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, console.MaskRecipient(tc.ch, tc.in))
		})
	}
}
