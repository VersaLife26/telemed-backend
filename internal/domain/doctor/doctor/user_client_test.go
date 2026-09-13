package doctor

import "testing"

// TestPermanentProvisionStatus pins which user-service replies an admin should
// be told to retry. Getting this wrong is not a crash, it is an operator
// pressing Approve forever on an application that can never provision -- or
// giving up on one that would have worked a second later.
func TestPermanentProvisionStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code      int
		permanent bool
		why       string
	}{
		{400, true, "unusable email or phone on the application"},
		{403, true, "the account exists but is suspended or deleted"},
		{404, true, "nothing to reconcile against"},
		{409, true, "email and phone belong to two different accounts"},
		{422, true, "validation"},
		{408, false, "request timeout means try again"},
		{425, false, "too early means try again"},
		{429, false, "rate limited means try again"},
		{500, false, "user-service is broken, not the data"},
		{502, false, "bad gateway"},
		{503, false, "user-service is down"},
		{504, false, "gateway timeout"},
	}
	for _, c := range cases {
		if got := permanentProvisionStatus(c.code); got != c.permanent {
			t.Errorf("permanentProvisionStatus(%d) = %v, want %v (%s)", c.code, got, c.permanent, c.why)
		}
	}
}
