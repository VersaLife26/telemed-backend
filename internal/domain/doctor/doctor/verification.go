package doctor

// VerificationAction is an admin-initiated transition on the credentialing
// workflow. Distinct from VerificationStatus: an action names an intent,
// a status names a state, and the mapping between them is not 1:1 -- reject
// and reopen both touch StatusRejected but from opposite directions.
type VerificationAction string

const (
	ActionStartReview VerificationAction = "start_review"
	ActionApprove     VerificationAction = "approve"
	ActionReject      VerificationAction = "reject"
	ActionReopen      VerificationAction = "reopen"
	ActionSuspend     VerificationAction = "suspend"
	ActionReinstate   VerificationAction = "reinstate"
)

// verificationTransitions is the whole state machine in one place. Reading
// it top to bottom is reading the credentialing workflow:
//
//	pending ------start_review-----> under_review
//	under_review ----approve--------> approved
//	under_review ----reject---------> rejected
//	approved --------suspend--------> suspended
//	suspended -------reinstate------> approved
//	rejected --------reopen---------> under_review
//
// The one rule the build brief calls out by name -- "you cannot approve an
// already-rejected doctor without an explicit reopen" -- falls out of this
// table for free: StatusRejected has no "approve" entry. The only edge out
// of rejected is reopen, which lands back in under_review, from which
// approve becomes legal again. There is no shortcut.
var verificationTransitions = map[VerificationStatus]map[VerificationAction]VerificationStatus{
	StatusPending: {
		ActionStartReview: StatusUnderReview,
		ActionApprove:     StatusApproved,
		ActionReject:      StatusRejected,
	},
	StatusUnderReview: {
		ActionApprove: StatusApproved,
		ActionReject:  StatusRejected,
	},
	StatusApproved: {
		ActionSuspend: StatusSuspended,
	},
	StatusSuspended: {
		ActionReinstate: StatusApproved,
	},
	StatusRejected: {
		ActionReopen: StatusUnderReview,
	},
}

// reasonRequiredActions lists actions where an admin must explain themselves.
// A rejection or suspension with no reason is not auditable and is not
// actionable by the doctor on the other end of it.
var reasonRequiredActions = map[VerificationAction]bool{
	ActionReject:  true,
	ActionSuspend: true,
}

// nextVerificationStatus resolves the target status for action from current,
// or returns ErrInvalidTransition if the workflow forbids it.
func nextVerificationStatus(current VerificationStatus, action VerificationAction) (VerificationStatus, error) {
	edges, ok := verificationTransitions[current]
	if !ok {
		return "", ErrInvalidTransition
	}
	next, ok := edges[action]
	if !ok {
		return "", ErrInvalidTransition
	}
	return next, nil
}
