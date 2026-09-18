package disputes

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/directory"
)

// Directory resolves a user id to the identity fields this service does not
// own. Only the existence and the role of the answer are used here; the names
// and phone numbers it also carries are deliberately not stored on a dispute.
//
// It is the narrow interface rather than *directory.GRPCClient so a test can
// substitute a fake, and so a future deployment could resolve identities some
// other way.
type Directory interface {
	User(ctx context.Context, userID uuid.UUID) (directory.User, error)
}

// RefundOpener records a pending finance refund when a dispute asks for money.
type RefundOpener interface {
	OpenFromDispute(ctx context.Context, disputeID, appointmentID uuid.UUID, amountCents *int64, reason string) error
}

// ErrUnknownParty means the patient or doctor named on a dispute does not
// exist, or is not the kind of user the field claims.
var ErrUnknownParty = errors.New("disputes: the named patient or doctor does not exist")

type Service struct {
	repo    *Repository
	dir     Directory
	refunds RefundOpener
}

// NewService takes the directory so Create can check who a dispute is about.
// A nil directory disables the check, which is only correct in a test that is
// about something else -- cmd/server always passes one.
func NewService(repo *Repository, dir Directory) *Service { return &Service{repo: repo, dir: dir} }

func (s *Service) SetRefundOpener(r RefundOpener) { s.refunds = r }

func (s *Service) Create(ctx context.Context, p CreateParams) (Dispute, error) {
	// SECURITY-REVIEW F23: patient_id and doctor_id arrived from the request
	// body, uuid4-shaped and otherwise unchecked, and were written straight
	// into a row that names a real clinician as the subject of a complaint.
	//
	// Two consequences, and the second is the one that matters. A dispute
	// against a doctor id that does not exist is noise a human has to
	// discover. A dispute against a doctor id that DOES exist, opened by any
	// of the five admin roles with an arbitrary patient id attached, is a
	// fabricated complaint about a named clinician on a professional record --
	// and this platform's disputes carry a refund_amount_cents, so it is also
	// a route to moving money to an unrelated account.
	//
	// The ids are checked against user-service's directory, which is the only
	// component that knows whether a user exists and what they are. A
	// directory outage fails the create rather than admitting an unverified
	// party: refusing to open a dispute for a few minutes is recoverable, an
	// unverifiable accusation on a doctor's record is not.
	if err := s.checkParties(ctx, p.PatientID, p.DoctorID); err != nil {
		return Dispute{}, err
	}

	d, err := s.repo.Create(ctx, p)
	if err != nil {
		return Dispute{}, err
	}
	if p.RefundRequested && s.refunds != nil {
		if err := s.refunds.OpenFromDispute(ctx, d.ID, d.AppointmentID, d.RefundAmountCents, d.Description); err != nil {
			return Dispute{}, err
		}
	}
	audit.Stage(ctx, audit.Draft{
		Action: "dispute.opened", ResourceType: "dispute", ResourceID: d.ID.String(),
		NewValue: map[string]any{"category": d.Category, "refund_requested": d.RefundRequested},
	})
	return d, nil
}

// checkParties resolves both ids and asserts each is the kind of user the
// field claims. It also rejects a dispute where the patient and the doctor are
// the same identity, which is never a real complaint.
func (s *Service) checkParties(ctx context.Context, patientID, doctorID uuid.UUID) error {
	if s.dir == nil {
		return nil
	}
	if patientID == uuid.Nil || doctorID == uuid.Nil {
		return fmt.Errorf("%w: both patient_id and doctor_id are required", ErrUnknownParty)
	}
	if patientID == doctorID {
		return fmt.Errorf("%w: patient_id and doctor_id are the same identity", ErrUnknownParty)
	}
	for _, party := range []struct {
		field    string
		id       uuid.UUID
		wantRole string
	}{
		{"patient_id", patientID, "patient"},
		{"doctor_id", doctorID, "doctor"},
	} {
		u, err := s.dir.User(ctx, party.id)
		if errors.Is(err, directory.ErrNotFound) {
			return fmt.Errorf("%w: %s %s is not a user of this platform", ErrUnknownParty, party.field, party.id)
		}
		if err != nil {
			// Not ErrUnknownParty: the directory is unreachable, which is a
			// 503 the caller should retry, not a 422 telling them their input
			// was wrong.
			return fmt.Errorf("disputes: resolve %s: %w", party.field, err)
		}
		if u.Role != "" && u.Role != party.wantRole {
			return fmt.Errorf("%w: %s %s is a %s, not a %s", ErrUnknownParty, party.field, party.id, u.Role, party.wantRole)
		}
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Dispute, error) { return s.repo.Get(ctx, id) }

func (s *Service) List(ctx context.Context, f ListFilter) ([]Dispute, int64, error) {
	return s.repo.List(ctx, f)
}

// Actor is the authenticated admin performing a dispute action. It is passed
// explicitly rather than pulled from the context inside the service, so the
// rule below is visible in the signature and cannot be forgotten by a future
// caller.
//
// CanForce is true only for super_admin. Without an override, a dispute
// assigned to an admin who has left the company would be unresolvable
// forever -- so enforcing the assignee without an escape hatch would trade a
// security hole for an operational one. A forced action is permitted, and it
// is recorded under a DIFFERENT audit action (dispute.force_resolved /
// dispute.force_reassigned) so it stands out in a review instead of blending
// into ordinary traffic.
type Actor struct {
	ID       uuid.UUID
	CanForce bool
}

// mayAct reports whether actor is allowed to act on d: nobody has claimed it,
// they claimed it themselves, or they are overriding deliberately.
func mayAct(d Dispute, actor Actor) bool {
	return actor.CanForce || d.AssignedTo == nil || *d.AssignedTo == actor.ID
}

func (s *Service) Assign(ctx context.Context, id, assignee uuid.UUID, actor Actor, version int) (Dispute, error) {
	before, err := s.repo.Get(ctx, id)
	if err != nil {
		return Dispute{}, err
	}
	if !mayAct(before, actor) {
		return Dispute{}, ErrNotAssignee
	}

	forced := actor.CanForce && before.AssignedTo != nil && *before.AssignedTo != actor.ID

	after, err := s.repo.Assign(ctx, id, assignee, actor.ID, version, forced)
	if err != nil {
		return Dispute{}, s.classify(ctx, id, actor, err)
	}

	action := "dispute.assigned"
	if forced {
		action = "dispute.force_reassigned"
	}
	audit.Stage(ctx, audit.Draft{
		Action: action, ResourceType: "dispute", ResourceID: id.String(),
		OldValue: map[string]any{"assigned_to": before.AssignedTo, "status": before.Status},
		NewValue: map[string]any{"assigned_to": after.AssignedTo, "status": after.Status, "forced": forced},
	})
	return after, nil
}

// classify turns the repository's bare "no row matched" into the error that
// actually explains it. The UPDATE predicate covers both the optimistic lock
// and the assignee, so a miss means one of the two moved between the read and
// the write. Re-reading is the only way to tell them apart, and telling them
// apart matters: a 409 invites a retry, a 403 does not.
func (s *Service) classify(ctx context.Context, id uuid.UUID, actor Actor, err error) error {
	if !errors.Is(err, ErrVersionConflict) {
		return err
	}
	current, getErr := s.repo.Get(ctx, id)
	if getErr != nil {
		return err
	}
	if !mayAct(current, actor) {
		return ErrNotAssignee
	}
	return err
}

func (s *Service) Comment(ctx context.Context, disputeID, authorID uuid.UUID, body string) (Comment, error) {
	if _, err := s.repo.Get(ctx, disputeID); err != nil {
		return Comment{}, err
	}
	c, err := s.repo.AddComment(ctx, disputeID, authorID, body)
	if err != nil {
		return Comment{}, err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "dispute.commented", ResourceType: "dispute", ResourceID: disputeID.String(),
		NewValue: map[string]any{"comment_id": c.ID},
	})
	return c, nil
}

func (s *Service) Comments(ctx context.Context, disputeID uuid.UUID) ([]Comment, error) {
	return s.repo.ListComments(ctx, disputeID)
}

func (s *Service) Resolve(ctx context.Context, id uuid.UUID, resolution string, refundAmountCents *int64, actor Actor, version int) (Dispute, error) {
	before, err := s.repo.Get(ctx, id)
	if err != nil {
		return Dispute{}, err
	}
	if !mayAct(before, actor) {
		return Dispute{}, ErrNotAssignee
	}

	forced := actor.CanForce && before.AssignedTo != nil && *before.AssignedTo != actor.ID

	after, err := s.repo.Resolve(ctx, id, resolution, refundAmountCents, actor.ID, version, forced)
	if err != nil {
		return Dispute{}, s.classify(ctx, id, actor, err)
	}

	action := "dispute.resolved"
	if forced {
		action = "dispute.force_resolved"
	}
	audit.Stage(ctx, audit.Draft{
		Action: action, ResourceType: "dispute", ResourceID: id.String(),
		OldValue: map[string]any{"status": before.Status, "assigned_to": before.AssignedTo},
		NewValue: map[string]any{
			"status": after.Status, "resolution": after.Resolution,
			"refund_amount_cents": after.RefundAmountCents, "forced": forced,
		},
	})
	return after, nil
}
