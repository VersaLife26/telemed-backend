package disputes

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/directory"
)

// mayAct is the whole of finding F23 expressed as one predicate: the disputes
// table has carried an assigned_to column since migrations/000003, /assign has
// been exposed since the same commit, and nothing anywhere read it. Any of the
// five admin roles could resolve any dispute -- deciding whether a patient gets
// their money back -- or quietly reassign one away from the person working it.
//
// The table below is the authorization rule itself, tested directly, because
// the SQL predicate and the service check must agree and both derive from this.
func TestMayAct_EnforcesTheAssignee(t *testing.T) {
	holder := uuid.New()
	other := uuid.New()

	assignedTo := func(id uuid.UUID) Dispute { return Dispute{AssignedTo: &id} }

	cases := []struct {
		name    string
		dispute Dispute
		actor   Actor
		want    bool
	}{
		{
			name:    "nobody has claimed it: anyone may act",
			dispute: Dispute{},
			actor:   Actor{ID: other},
			want:    true,
		},
		{
			name:    "the assignee may act on their own dispute",
			dispute: assignedTo(holder),
			actor:   Actor{ID: holder},
			want:    true,
		},
		{
			name:    "another admin may NOT act on someone else's dispute",
			dispute: assignedTo(holder),
			actor:   Actor{ID: other},
			want:    false,
		},
		{
			name:    "another admin may not act even holding the force flag without the right",
			dispute: assignedTo(holder),
			actor:   Actor{ID: other, CanForce: false},
			want:    false,
		},
		{
			name:    "a super_admin overriding deliberately may act",
			dispute: assignedTo(holder),
			actor:   Actor{ID: other, CanForce: true},
			want:    true,
		},
		{
			name: "an unresolved actor id is not treated as a wildcard",
			// adminusers.FromContext returning nothing yields a zero UUID.
			// A dispute assigned to a real admin must not become actionable
			// just because the caller could not be resolved.
			dispute: assignedTo(holder),
			actor:   Actor{ID: uuid.Nil},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, mayAct(tc.dispute, tc.actor))
		})
	}
}

// --- F23, second half: the parties on a dispute were never checked ---------

// stubDirectory answers from a fixed table so a test can say "this id exists
// and is a doctor" without a running user-service.
type stubDirectory struct {
	users map[uuid.UUID]directory.User
	err   error
}

func (d stubDirectory) User(_ context.Context, id uuid.UUID) (directory.User, error) {
	if d.err != nil {
		return directory.User{}, d.err
	}
	u, ok := d.users[id]
	if !ok {
		return directory.User{}, fmt.Errorf("%w: %s", directory.ErrNotFound, id)
	}
	return u, nil
}

// POST /admin/disputes took patient_id and doctor_id from the request body,
// uuid4-shaped and otherwise unchecked, and wrote them into a row that names a
// real clinician as the subject of a complaint carrying a
// refund_amount_cents. Any of the five admin roles could therefore fabricate a
// complaint against any doctor, or attach a patient who has nothing to do with
// the appointment.
func TestCreate_ValidatesBothPartiesAgainstTheDirectory(t *testing.T) {
	patientID, doctorID := uuid.New(), uuid.New()
	dir := stubDirectory{users: map[uuid.UUID]directory.User{
		patientID: {UserID: patientID, Role: "patient"},
		doctorID:  {UserID: doctorID, Role: "doctor"},
	}}
	svc := NewService(nil, dir)

	cases := []struct {
		name      string
		patient   uuid.UUID
		doctor    uuid.UUID
		wantParty bool
	}{
		{"a doctor id nobody holds", patientID, uuid.New(), true},
		{"a patient id nobody holds", uuid.New(), doctorID, true},
		{"neither exists", uuid.New(), uuid.New(), true},
		{"the doctor field naming a patient", patientID, patientID, true},
		{"the patient field naming a doctor", doctorID, doctorID, true},
		{"a missing patient id", uuid.Nil, doctorID, true},
		{"a missing doctor id", patientID, uuid.Nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.checkParties(context.Background(), tc.patient, tc.doctor)
			require.Error(t, err,
				"a dispute was opened naming patient=%s doctor=%s; neither was checked against the directory",
				tc.patient, tc.doctor)
			require.ErrorIs(t, err, ErrUnknownParty)
		})
	}

	// The legitimate case still works.
	require.NoError(t, svc.checkParties(context.Background(), patientID, doctorID))
}

// A directory outage must not admit an unverified party. Refusing to open a
// dispute for a few minutes is recoverable; an unverifiable accusation on a
// clinician's professional record is not. It must also be distinguishable from
// "your input was wrong", so it is deliberately NOT ErrUnknownParty.
func TestCreate_ADirectoryOutageFailsClosed(t *testing.T) {
	svc := NewService(nil, stubDirectory{err: errors.New("grpc: connection refused")})
	err := svc.checkParties(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err, "a dispute was opened while the directory was unreachable")
	require.NotErrorIs(t, err, ErrUnknownParty,
		"an outage must not be reported to the caller as bad input; it is retryable")
}
