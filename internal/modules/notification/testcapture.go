package notification

import (
	"context"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/platform/testkit"
)

// capturingProvider records a rendered notification in the test outbox instead
// of delivering it.
//
// Unlike the console provider it stands in for, this one keeps the content
// somewhere a developer can read it back over HTTP rather than only in stdout,
// which is what makes "did the appointment-confirmed email render correctly"
// answerable from the test page.
//
// in_app is deliberately NOT captured: that channel's delivery is writing the
// notification row the user's own notification list reads, so replacing it
// with a capture would make the feature under test stop working.
type capturingProvider struct {
	inner    notification.NotificationProvider
	outbox   *testkit.Outbox
	channels []notification.Channel
}

var _ notification.NotificationProvider = (*capturingProvider)(nil)

func newCapturingProvider(inner notification.NotificationProvider, outbox *testkit.Outbox, chs []notification.Channel) *capturingProvider {
	return &capturingProvider{inner: inner, outbox: outbox, channels: chs}
}

func (c *capturingProvider) Send(_ context.Context, msg notification.Message) (notification.Receipt, error) {
	to := msg.To
	if msg.Channel == notification.ChannelPush {
		to = msg.DeviceToken
	}
	rec := c.outbox.Record(testkit.Message{
		Kind:     kindFor(msg.Channel),
		To:       to,
		Subject:  msg.Subject,
		Body:     msg.Body,
		Provider: "captured (would have used " + c.inner.Name() + ")",
	})
	// Delivered is true because from the dispatcher's point of view this send
	// succeeded and must not be retried. Reporting it as undelivered would put
	// every test-mode notification into the retry pipeline forever.
	return notification.Receipt{ProviderMessageID: "captured-" + rec.ID, Delivered: true}, nil
}

func (c *capturingProvider) Channels() []notification.Channel { return c.channels }
func (c *capturingProvider) Name() string                     { return "captured" }

func kindFor(ch notification.Channel) testkit.Kind {
	switch ch {
	case notification.ChannelEmail:
		return testkit.KindEmail
	case notification.ChannelPush:
		return testkit.KindPush
	case notification.ChannelInApp:
		return testkit.KindInApp
	default:
		return testkit.KindSMS
	}
}
