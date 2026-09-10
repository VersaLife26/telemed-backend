package user

import (
	"context"

	"telemed/internal/domain/user/user"
	"telemed/internal/platform/testkit"
)

// capturingSMS records every outbound SMS in the test outbox and does not
// deliver it.
//
// It REPLACES the real provider rather than wrapping it, and that is the point
// rather than a shortcut. An OTP is a bearer credential for the account it
// belongs to; if test mode both captured and delivered, a developer poking at
// the test page would be sending real messages to whatever number they typed,
// and a typo'd digit is an OTP on a stranger's handset. Capture is the whole
// behaviour: nothing leaves the process.
//
// It is only ever constructed when config.Base.TestModeEnabled is true, which
// production overrides regardless of the flag.
type capturingSMS struct {
	outbox   *testkit.Outbox
	replaced string
}

var _ user.SMSProvider = (*capturingSMS)(nil)

// newCapturingSMS returns the provider that stands in for real delivery in
// test mode. replaced names the provider it displaced, purely so the test page
// can show what would have carried the message.
func newCapturingSMS(outbox *testkit.Outbox, replaced string) *capturingSMS {
	return &capturingSMS{outbox: outbox, replaced: replaced}
}

func (c *capturingSMS) Send(_ context.Context, phone, body string) (string, error) {
	msg := c.outbox.Record(testkit.Message{
		Kind:     testkit.KindSMS,
		To:       phone,
		Body:     body,
		Provider: "captured (would have used " + c.replaced + ")",
	})
	return "captured-" + msg.ID, nil
}
