package notification

import (
	"testing"
	"time"
)

// TestDecideDispatch_PreferenceSuppression covers the "respect preferences"
// half of the delivery semantics: a channel the user has switched off is
// suppressed, except for critical (otp_code) which cannot be opted out of --
// a user cannot disable the SMS channel and then be unable to log in.
func TestDecideDispatch_PreferenceSuppression(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, loc) // midday: never in quiet hours

	cases := []struct {
		name       string
		urgency    Urgency
		prefEnable bool
		wantStatus Status
	}{
		{"normal, channel disabled -> suppressed", UrgencyNormal, false, StatusSuppressed},
		{"normal, channel enabled -> queued", UrgencyNormal, true, StatusQueued},
		{"urgent, channel disabled -> suppressed", UrgencyUrgent, false, StatusSuppressed},
		{"urgent, channel enabled -> queued", UrgencyUrgent, true, StatusQueued},
		{"critical, channel disabled -> still queued (cannot opt out of OTP)", UrgencyCritical, false, StatusQueued},
		{"critical, channel enabled -> queued", UrgencyCritical, true, StatusQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, scheduledFor := decideDispatch(now, loc, ChannelSMS, tc.urgency, tc.prefEnable, true, nil, nil, false)
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if scheduledFor != nil {
				t.Errorf("scheduledFor should be nil outside quiet hours, got %v", *scheduledFor)
			}
		})
	}
}

func TestDecideDispatch_MissingRecipientSuppresses(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, loc)

	// hasRecipient=false models an sms/email send where the triggering
	// event carried no phone/email -- suppress rather than hand an empty
	// string to a provider and pay for a guaranteed failure.
	status, _ := decideDispatch(now, loc, ChannelSMS, UrgencyNormal, true, false, nil, nil, false)
	if status != StatusSuppressed {
		t.Errorf("missing recipient: status = %q, want %q", status, StatusSuppressed)
	}

	// Even critical (OTP) cannot be sent with nowhere to send it.
	status = firstOf(decideDispatch(now, loc, ChannelSMS, UrgencyCritical, true, false, nil, nil, false))
	if status != StatusSuppressed {
		t.Errorf("missing recipient, critical: status = %q, want %q", status, StatusSuppressed)
	}
}

func firstOf(s Status, _ *time.Time) Status { return s }

func TestDecideDispatch_QuietHoursDefersNormalNotUrgent(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	quietStart, quietEnd := dur(22, 0), dur(6, 0)
	now := time.Date(2026, 8, 20, 23, 0, 0, 0, loc) // inside quiet hours

	// Normal urgency SMS during quiet hours: held, not suppressed.
	status, scheduledFor := decideDispatch(now, loc, ChannelSMS, UrgencyNormal, true, true, &quietStart, &quietEnd, false)
	if status != StatusQueued {
		t.Errorf("status = %q, want %q (deferred, still queued)", status, StatusQueued)
	}
	if scheduledFor == nil {
		t.Fatal("expected a scheduled_for instant when deferred by quiet hours")
	}
	wantEnd := time.Date(2026, 8, 21, 6, 0, 0, 0, loc)
	if !scheduledFor.Equal(wantEnd) {
		t.Errorf("scheduledFor = %v, want %v", scheduledFor.In(loc), wantEnd)
	}

	// The exact same conditions, but reminder_1h's urgent classification:
	// sent immediately, not deferred.
	status, scheduledFor = decideDispatch(now, loc, ChannelSMS, UrgencyUrgent, true, true, &quietStart, &quietEnd, false)
	if status != StatusQueued || scheduledFor != nil {
		t.Errorf("urgent during quiet hours: status=%q scheduledFor=%v, want queued/nil (not deferred)", status, scheduledFor)
	}
}

func TestDecideDispatch_ImmediateOnlyAppliesWhenNotDeferred(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, loc)

	status, _ := decideDispatch(now, loc, ChannelSMS, UrgencyCritical, true, true, nil, nil, true)
	if status != StatusSending {
		t.Errorf("immediate + not deferred: status = %q, want %q", status, StatusSending)
	}

	// Immediate during a deferral window must still be held -- there is no
	// such thing as "immediately, but later".
	quietStart, quietEnd := dur(0, 0), dur(23, 59)
	nowQuiet := time.Date(2026, 8, 20, 12, 0, 0, 0, loc)
	status, scheduledFor := decideDispatch(nowQuiet, loc, ChannelSMS, UrgencyNormal, true, true, &quietStart, &quietEnd, true)
	if status != StatusQueued || scheduledFor == nil {
		t.Errorf("immediate + deferred: status=%q scheduledFor=%v, want queued + non-nil", status, scheduledFor)
	}
}
