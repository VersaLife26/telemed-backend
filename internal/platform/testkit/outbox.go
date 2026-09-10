// Package testkit holds the developer test surface's shared state.
//
// The one thing in here is the outbox: an in-memory record of every message
// the platform tried to send while TELEMED_TEST_MODE was on. It exists so a
// developer can read back the OTP code they just triggered without an SMS
// account, and read the rendered body of an email without an SMTP server.
//
// It is memory only, never Postgres and never Redis, and that is deliberate.
// The outbox holds plaintext OTP codes and rendered clinical content. Anything
// that persists it -- a table, a Redis key, a log line shipped to an
// aggregator -- outlives the process that was allowed to have it. Restarting
// the binary is the retention policy.
package testkit

import (
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Kind is the transport a captured message was headed for.
type Kind string

const (
	KindSMS   Kind = "sms"
	KindEmail Kind = "email"
	KindPush  Kind = "push"
	KindInApp Kind = "in_app"
)

// Message is one captured send.
type Message struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	// Provider names the implementation that would have delivered this, so a
	// developer can tell "captured instead of sending" from "the console
	// provider was selected and would have dropped it anyway".
	Provider string `json:"provider"`
	// Code is the 6-digit sequence found in Body, lifted out so the test page
	// can show it without the reader parsing an SMS template by eye. Empty
	// when the body carries no such sequence.
	Code string    `json:"code,omitempty"`
	At   time.Time `json:"at"`
}

// otpPattern matches a standalone 6-digit run, which is what GenerateOTP
// produces and what every OTP template interpolates. Anchored on non-digits at
// both ends so a 10-digit reference number does not yield its middle six.
var otpPattern = regexp.MustCompile(`(^|\D)(\d{6})($|\D)`)

// ExtractCode returns the 6-digit code in body, or "" if there is not exactly
// one candidate. Ambiguity returns nothing rather than the first match: a
// wrong code shown confidently costs more debugging time than no code shown.
func ExtractCode(body string) string {
	matches := otpPattern.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return ""
	}
	return matches[0][2]
}

// Outbox is a bounded, concurrency-safe ring of captured messages.
type Outbox struct {
	mu       sync.Mutex
	capacity int
	seq      uint64
	msgs     []Message
}

// DefaultCapacity is how many messages an Outbox keeps before the oldest is
// dropped. Large enough for a debugging session, small enough that a load test
// against a test-mode process cannot exhaust memory.
const DefaultCapacity = 200

// NewOutbox returns an outbox holding at most capacity messages.
func NewOutbox(capacity int) *Outbox {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Outbox{capacity: capacity}
}

// Record stores m, filling in ID, At and Code, and returns the stored copy.
// A nil Outbox records nothing, so callers wired for a non-test process do not
// need a guard at every call site.
func (o *Outbox) Record(m Message) Message {
	if o == nil {
		return m
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	o.seq++
	m.ID = strconv.FormatUint(o.seq, 10)
	m.At = time.Now().UTC()
	if m.Code == "" {
		m.Code = ExtractCode(m.Body)
	}

	o.msgs = append(o.msgs, m)
	if len(o.msgs) > o.capacity {
		o.msgs = o.msgs[len(o.msgs)-o.capacity:]
	}
	return m
}

// List returns captured messages newest first. kind filters when non-empty;
// limit caps the result when positive.
func (o *Outbox) List(kind Kind, limit int) []Message {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	out := make([]Message, 0, len(o.msgs))
	for i := len(o.msgs) - 1; i >= 0; i-- {
		if kind != "" && o.msgs[i].Kind != kind {
			continue
		}
		out = append(out, o.msgs[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// Latest returns the most recent message for a recipient on a channel, which
// is the "what code did I just get" question the test page actually asks.
func (o *Outbox) Latest(kind Kind, to string) (Message, bool) {
	if o == nil {
		return Message{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	for i := len(o.msgs) - 1; i >= 0; i-- {
		if o.msgs[i].Kind == kind && o.msgs[i].To == to {
			return o.msgs[i], true
		}
	}
	return Message{}, false
}

// Clear drops everything captured so far.
func (o *Outbox) Clear() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = nil
}
