package testkit

import "testing"

func TestExtractCode(t *testing.T) {
	cases := []struct{ body, want string }{
		{"Your telemed code is 418302. It expires in 5 minutes.", "418302"},
		{"004821 is your code", "004821"},
		// Ambiguity yields nothing rather than a guess: showing the wrong six
		// digits confidently costs more debugging time than showing none.
		{"code 123456 or maybe 654321", ""},
		{"reference 1234567890", ""},
		{"no digits here", ""},
	}
	for _, c := range cases {
		if got := ExtractCode(c.body); got != c.want {
			t.Errorf("ExtractCode(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestOutbox_RecordsAndListsNewestFirst(t *testing.T) {
	o := NewOutbox(10)
	o.Record(Message{Kind: KindSMS, To: "+94771234567", Body: "code 111111"})
	o.Record(Message{Kind: KindEmail, To: "a@b.lk", Subject: "Hello", Body: "body"})

	all := o.List("", 0)
	if len(all) != 2 {
		t.Fatalf("got %d messages, want 2", len(all))
	}
	if all[0].Kind != KindEmail {
		t.Errorf("first message is %q, want the newest (%q)", all[0].Kind, KindEmail)
	}

	sms := o.List(KindSMS, 0)
	if len(sms) != 1 || sms[0].Code != "111111" {
		t.Fatalf("sms filter returned %+v", sms)
	}
}

func TestOutbox_LatestFindsTheMostRecentForARecipient(t *testing.T) {
	o := NewOutbox(10)
	o.Record(Message{Kind: KindSMS, To: "+9477", Body: "code 111111"})
	o.Record(Message{Kind: KindSMS, To: "+9478", Body: "code 222222"})
	o.Record(Message{Kind: KindSMS, To: "+9477", Body: "code 333333"})

	got, ok := o.Latest(KindSMS, "+9477")
	if !ok {
		t.Fatal("Latest found nothing")
	}
	if got.Code != "333333" {
		t.Errorf("code %q, want the most recent (333333)", got.Code)
	}
	if _, ok := o.Latest(KindSMS, "+9479"); ok {
		t.Error("Latest invented a message for an unknown recipient")
	}
}

// TestOutbox_IsBounded matters because the outbox holds plaintext OTP codes in
// memory: an unbounded one is both a leak and a way to exhaust the process.
func TestOutbox_IsBounded(t *testing.T) {
	o := NewOutbox(3)
	for i := 0; i < 10; i++ {
		o.Record(Message{Kind: KindSMS, To: "+9477", Body: "x"})
	}
	if got := len(o.List("", 0)); got != 3 {
		t.Fatalf("outbox holds %d messages, want the 3 it was capped at", got)
	}
}

// A nil Outbox is the not-in-test-mode case, and every call site relies on it
// being inert rather than guarding individually.
func TestOutbox_NilIsInert(t *testing.T) {
	var o *Outbox
	o.Record(Message{Kind: KindSMS, To: "+9477", Body: "x"})
	o.Clear()
	if got := o.List("", 0); got != nil {
		t.Errorf("nil outbox returned %+v", got)
	}
	if _, ok := o.Latest(KindSMS, "+9477"); ok {
		t.Error("nil outbox reported a message")
	}
}
