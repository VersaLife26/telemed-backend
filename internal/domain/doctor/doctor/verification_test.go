package doctor

import (
	"errors"
	"testing"
)

// TestVerificationTransitions is table-driven over every (status, action)
// pair the workflow claims to support, plus a representative sample of
// forbidden ones. The case the build brief calls out by name -- "cannot
// approve an already-rejected doctor without an explicit reopen" -- is
// asserted explicitly, along with the reopen path that makes approval legal
// again.
func TestVerificationTransitions(t *testing.T) {
	tests := []struct {
		name    string
		current VerificationStatus
		action  VerificationAction
		want    VerificationStatus
		wantErr bool
	}{
		// --- legal transitions ---
		{"pending start_review", StatusPending, ActionStartReview, StatusUnderReview, false},
		{"pending approve direct", StatusPending, ActionApprove, StatusApproved, false},
		{"pending reject direct", StatusPending, ActionReject, StatusRejected, false},
		{"under_review approve", StatusUnderReview, ActionApprove, StatusApproved, false},
		{"under_review reject", StatusUnderReview, ActionReject, StatusRejected, false},
		{"approved suspend", StatusApproved, ActionSuspend, StatusSuspended, false},
		{"suspended reinstate", StatusSuspended, ActionReinstate, StatusApproved, false},
		{"rejected reopen", StatusRejected, ActionReopen, StatusUnderReview, false},

		// --- the rule the brief calls out by name ---
		{"REJECTED CANNOT BE APPROVED DIRECTLY", StatusRejected, ActionApprove, "", true},
		{"rejected cannot be rejected again", StatusRejected, ActionReject, "", true},
		{"rejected cannot skip straight to approved via suspend path", StatusRejected, ActionSuspend, "", true},

		// --- other forbidden transitions ---
		{"pending cannot suspend", StatusPending, ActionSuspend, "", true},
		{"pending cannot reinstate", StatusPending, ActionReinstate, "", true},
		{"pending cannot reopen", StatusPending, ActionReopen, "", true},
		{"under_review cannot start_review again", StatusUnderReview, ActionStartReview, "", true},
		{"under_review cannot reopen", StatusUnderReview, ActionReopen, "", true},
		{"approved cannot approve again", StatusApproved, ActionApprove, "", true},
		{"approved cannot reject", StatusApproved, ActionReject, "", true},
		{"approved cannot reopen", StatusApproved, ActionReopen, "", true},
		{"suspended cannot approve directly", StatusSuspended, ActionApprove, "", true},
		{"suspended cannot suspend again", StatusSuspended, ActionSuspend, "", true},
		{"unknown status rejects everything", VerificationStatus("bogus"), ActionApprove, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextVerificationStatus(tt.current, tt.action)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("nextVerificationStatus(%s, %s) = %s, nil; want error", tt.current, tt.action, got)
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("expected ErrInvalidTransition, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("nextVerificationStatus(%s, %s) unexpected error: %v", tt.current, tt.action, err)
			}
			if got != tt.want {
				t.Fatalf("nextVerificationStatus(%s, %s) = %s, want %s", tt.current, tt.action, got, tt.want)
			}
		})
	}
}

// TestVerificationRejectedRequiresReopenBeforeApproval walks the full
// sequence end to end: reject, confirm approve is now illegal, reopen,
// confirm approve is legal again. This is the scenario in prose, not just
// the single-hop table above.
func TestVerificationRejectedRequiresReopenBeforeApproval(t *testing.T) {
	status := StatusUnderReview

	status, err := nextVerificationStatus(status, ActionReject)
	if err != nil || status != StatusRejected {
		t.Fatalf("reject failed: status=%s err=%v", status, err)
	}

	if _, err := nextVerificationStatus(status, ActionApprove); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected approving a rejected doctor to fail, got status change with err=%v", err)
	}

	status, err = nextVerificationStatus(status, ActionReopen)
	if err != nil || status != StatusUnderReview {
		t.Fatalf("reopen failed: status=%s err=%v", status, err)
	}

	status, err = nextVerificationStatus(status, ActionApprove)
	if err != nil || status != StatusApproved {
		t.Fatalf("approve after reopen failed: status=%s err=%v", status, err)
	}
}

// TestReasonRequiredActions locks down which actions demand an explanation.
// A reject or suspend with no reason is not auditable.
func TestReasonRequiredActions(t *testing.T) {
	tests := []struct {
		action VerificationAction
		want   bool
	}{
		{ActionReject, true},
		{ActionSuspend, true},
		{ActionApprove, false},
		{ActionReopen, false},
		{ActionStartReview, false},
		{ActionReinstate, false},
	}
	for _, tt := range tests {
		if got := reasonRequiredActions[tt.action]; got != tt.want {
			t.Errorf("reasonRequiredActions[%s] = %v, want %v", tt.action, got, tt.want)
		}
	}
}
