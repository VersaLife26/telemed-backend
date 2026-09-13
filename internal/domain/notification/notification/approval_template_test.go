package notification

import (
	"testing"

	"telemed/internal/platform/events"
)

// TestApprovalTemplate covers the one decision this consumer makes on behalf
// of an approved doctor: which credential the email tells them to sign in
// with. Every wrong answer here ends with a doctor at a sign-in screen that
// rejects them, on the day they were approved.
func TestApprovalTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		event events.DoctorApplicationApproved
		want  TemplateKey
	}{
		{
			name:  "new account created with the apply password",
			event: events.DoctorApplicationApproved{LoginReady: true, PasswordApplied: true},
			want:  TemplateDoctorApplicationApproved,
		},
		{
			name:  "account already existed and kept its own password",
			event: events.DoctorApplicationApproved{LoginReady: true, PasswordApplied: false},
			want:  TemplateDoctorApplicationApprovedExisting,
		},
		{
			name:  "no login provisioned, OTP is the only way in",
			event: events.DoctorApplicationApproved{LoginReady: false, PasswordApplied: false},
			want:  TemplateDoctorApplicationApprovedOTP,
		},
		{
			// An event published before login_ready existed decodes as false.
			// It must fall to the OTP wording, which was true at the time it
			// was written, rather than promise a password.
			name:  "pre-upgrade event with neither flag set",
			event: events.DoctorApplicationApproved{},
			want:  TemplateDoctorApplicationApprovedOTP,
		},
		{
			// Nonsense combination: no login, but a password was applied.
			// Nothing produces it; if something ever did, the safe reading is
			// still "you have no login yet".
			name:  "password applied without a login is not a sign-in promise",
			event: events.DoctorApplicationApproved{LoginReady: false, PasswordApplied: true},
			want:  TemplateDoctorApplicationApprovedOTP,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := approvalTemplate(c.event); got != c.want {
				t.Errorf("approvalTemplate() = %q, want %q", got, c.want)
			}
		})
	}
}
