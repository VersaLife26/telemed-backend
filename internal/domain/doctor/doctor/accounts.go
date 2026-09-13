package doctor

import (
	"context"

	"github.com/google/uuid"
)

// AccountProvisioner creates the platform login for an approved application.
//
// Doctor-service does not own users (ADR-004). After an admin approves, this
// seam asks user-service to insert a role=doctor row so the applicant can
// sign in with the email and password they chose on the apply form.
type AccountProvisioner interface {
	ProvisionDoctor(ctx context.Context, in DoctorAccount) (ProvisionResult, error)
}

// DoctorAccount is the identity copied from an approved public application.
type DoctorAccount struct {
	Email        string
	Phone        string
	Name         string
	PasswordHash string
}

// ProvisionResult is what user-service did with that identity.
//
// PasswordApplied is false when the applicant already had an account whose
// password user-service refused to overwrite. The approval email branches on
// it: telling that doctor to sign in with the password they just chose would
// send them round a loop, because it is not the password on their account.
type ProvisionResult struct {
	UserID          uuid.UUID
	PasswordApplied bool
}
