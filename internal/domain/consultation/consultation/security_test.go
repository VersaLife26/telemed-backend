package consultation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/middleware"
)

// Regression tests for the security review's F3 and F4 against this repo.
//
// Each one is written to fail against the code as it stood before the fix. The
// comment on each says which line to revert to see it fail.

// --- F3: a treating relationship needs a consultation that happened ---------

// TestAdmitRequiresThePatientToHaveAttended is F3.
//
// Admit is the only publisher of consultation.started, and record-service turns
// that event into a treating_relationships row -- permanent, unrevocable read
// and download on the patient's whole medical vault. Admit used to accept
// StatusScheduled, so the doctor needed nothing from the patient beyond a
// booking:
//
//	Dr X advertises a free two-minute consultation. Patient A books. X calls
//	admit, then end. A never opens the app. X now reads A's records forever.
//
// Reverting the gate to `c.Status != StatusScheduled && c.Status != StatusWaiting`
// makes the first half of this test succeed where it must fail.
func TestAdmitRequiresThePatientToHaveAttended(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	doctor := doctorPrincipal(uuid.New(), doctorID)

	// The exploit, exactly as written in the finding.
	_, err := svc.Admit(ctx, doctor, c.ID)
	require.ErrorIs(t, err, ErrInvalidState,
		"a doctor admitted a consultation the patient has never attended")

	// The refusal has to be real, not cosmetic. started_at is the field
	// record-service reads as "the relationship opened", so it must still be
	// unset, and the consultation must still be scheduled.
	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusScheduled, stored.Status)
	require.Nil(t, stored.StartedAt, "started_at was stamped for a consultation nobody attended")

	// And no admission on the timeline, which is the local record of the same
	// fact the event carries.
	for _, e := range st.timeline() {
		require.NotEqual(t, EventAdmitted, e.Type,
			"an admission was recorded for a consultation the patient never joined")
	}

	// The doctor still cannot get there by ending and retrying, or by any other
	// call they can make alone: only the patient's own token moves the
	// consultation to waiting.
	_, err = svc.Admit(ctx, doctor, c.ID)
	require.ErrorIs(t, err, ErrInvalidState)

	// The patient joins with their own credentials. Now, and only now, admit
	// works and the relationship may open.
	patientJoins(t, svc, patientID, c)
	admitted, err := svc.Admit(ctx, doctor, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, admitted.Status)
	require.NotNil(t, admitted.StartedAt)
}

// TestOnlyThePatientsOwnTokenReachesWaiting closes the obvious way around F3:
// if a doctor could drive the consultation to "waiting" themselves, the status
// gate would be theatre.
func TestOnlyThePatientsOwnTokenReachesWaiting(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	// The doctor joining the room does NOT put the patient in the waiting room.
	_, err := svc.Join(ctx, doctorPrincipal(uuid.New(), doctorID), c.AppointmentID)
	require.NoError(t, err)

	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusScheduled, stored.Status,
		"a doctor's own join moved the consultation to waiting, which would re-open F3")

	// Neither does a stranger's, nor an admin's -- Join has no admin path at
	// all, which is correct: nobody joins a consultation on someone's behalf.
	_, err = svc.Join(ctx, patientPrincipal(uuid.New()), c.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)

	admin := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleSuperAdmin}}
	_, err = svc.Join(ctx, admin, c.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)

	stored, err = st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusScheduled, stored.Status)
}

// TestJoin_ForeignPatientIsForbidden is the room-security assertion for this
// work: authorizeParty only admits the appointment's own patient or doctor,
// so a different patient must get 403 on join. Room security is already
// enforced server-side; this test pins that it stays that way.
func TestJoin_ForeignPatientIsForbidden(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)

	_, err := svc.Join(context.Background(), patientPrincipal(uuid.New()), c.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)
}

// TestZeroIdentityIsNeverAParty pins the guard that keeps authorizeParty honest.
//
// It is the same class of bug as scheduling's "an empty actor_id means admin":
// a principal with no id must not match a consultation with no doctor id.
// Dropping either `!= uuid.Nil` in authorizeParty fails this.
func TestZeroIdentityIsNeverAParty(t *testing.T) {
	c := &Consultation{ID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New()}
	if _, ok := authorizeParty(middleware.Principal{}, c); ok {
		t.Fatal("an empty principal was treated as a party")
	}

	// A consultation whose ids somehow arrived zeroed must not match a zeroed
	// principal either -- two unknowns are not a match.
	blank := &Consultation{ID: uuid.New()}
	if _, ok := authorizeParty(middleware.Principal{}, blank); ok {
		t.Fatal("uuid.Nil matched uuid.Nil and became a treating relationship")
	}
}

// --- F4: the admin sees the envelope, never the recording ------------------

// TestAdminReadsTheEnvelopeNotTheRecording is F4 for this repo.
//
// GET /api/v1/consultations/{id} is auth: "authenticated" at the gateway -- no
// role gate, so no IP allowlist and no admin-origin check -- and Service.Get
// returned the row wholesale to any of the five admin roles, recording_url
// included. That is a presigned link to the audiovisual record of a medical
// consultation, reachable from any IP on the internet.
//
// Removing the redaction block from Get fails the first sub-test.
func TestAdminReadsTheEnvelopeNotTheRecording(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	// Give the consultation a completed recording, as a real ended call has.
	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	url := "https://minio.internal/recordings/" + c.AppointmentID.String() + ".mp4?X-Amz-Signature=deadbeef"
	egress := "EG_abc123"
	ended := time.Now().UTC()
	dur := 900
	stored.RecordingURL = &url
	stored.EgressID = &egress
	stored.RecordingStatus = RecordingCompleted
	stored.Status = StatusEnded
	stored.EndedAt = &ended
	stored.DurationSeconds = &dur
	require.NoError(t, st.UpdateConsultation(ctx, fakeTx{}, stored))

	for _, role := range middleware.AdminRoles {
		t.Run(string(role)+" gets no recording", func(t *testing.T) {
			admin := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{role}}
			got, participants, err := svc.Get(ctx, admin, c.ID)
			require.NoError(t, err, "an admin must still be able to resolve a dispute")
			require.Nil(t, got.RecordingURL,
				"role %s was handed a link to the recording of a medical consultation", role)
			require.Nil(t, got.EgressID,
				"role %s was handed the egress id, which is a second route to the same object", role)

			// Everything a dispute actually needs is still there.
			require.Equal(t, StatusEnded, got.Status)
			require.NotNil(t, got.EndedAt)
			require.NotNil(t, got.DurationSeconds)
			require.Equal(t, RecordingCompleted, got.RecordingStatus,
				"an admin must still see THAT a recording exists, so they can request it through the audited route")
			require.NotNil(t, participants)
		})
	}

	t.Run("the patient still gets their own recording", func(t *testing.T) {
		got, _, err := svc.Get(ctx, patientPrincipal(patientID), c.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RecordingURL, "a party was denied their own consultation's recording")
		require.Equal(t, url, *got.RecordingURL)
	})

	t.Run("the doctor still gets it", func(t *testing.T) {
		got, _, err := svc.Get(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RecordingURL)
	})

	t.Run("a stranger gets nothing at all", func(t *testing.T) {
		_, _, err := svc.Get(ctx, patientPrincipal(uuid.New()), c.ID)
		require.ErrorIs(t, err, ErrForbidden)
	})
}

// TestRedactionDoesNotMutateTheStoredRow guards the copy in Get. An in-place
// nil would erase the recording for the patient who owns it the moment an
// admin looked at the consultation first, if anything ever caches the row.
func TestRedactionDoesNotMutateTheStoredRow(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	url := "https://minio.internal/recordings/x.mp4"
	stored.RecordingURL = &url
	stored.RecordingStatus = RecordingCompleted
	require.NoError(t, st.UpdateConsultation(ctx, fakeTx{}, stored))

	admin := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleSupport}}
	_, _, err = svc.Get(ctx, admin, c.ID)
	require.NoError(t, err)

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.NotNil(t, after.RecordingURL, "the redaction erased the stored recording url")
}

// TestDoctorMayOpenTheRoomBeforeThePatientArrives confirms the UX consequence
// of the F3 gate rather than assuming it, because record-service's half asked.
//
// A doctor who opens the consultation early is not blocked: Join succeeds,
// creates the LiveKit room, mints their token and records their participant
// row. Only Admit is refused, and only until the patient arrives. That is the
// ordering the waiting-room model already assumes -- the queue exists so a
// doctor admits FROM it -- so the gate adds a retry, not a new workflow.
func TestDoctorMayOpenTheRoomBeforeThePatientArrives(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	doctor := doctorPrincipal(uuid.New(), doctorID)

	// The doctor arrives first. Everything they need works.
	joined, err := svc.Join(ctx, doctor, c.AppointmentID)
	require.NoError(t, err, "a doctor must be able to open the room before the patient")
	require.NotEmpty(t, joined.Token)
	require.Equal(t, c.RoomName, joined.RoomName)
	require.Equal(t, string(RoleDoctor), joined.Role)
	require.Equal(t, StatusScheduled, joined.Status,
		"the doctor's own join must not promote the consultation to waiting")

	// Admit is the one thing they must wait for.
	_, err = svc.Admit(ctx, doctor, c.ID)
	require.ErrorIs(t, err, ErrInvalidState)

	// The patient arrives. The doctor's second attempt succeeds -- one retry,
	// no new workflow.
	patientJoins(t, svc, patientID, c)

	status, err := svc.WaitingRoomStatus(ctx, doctor, c.ID)
	require.NoError(t, err)
	require.True(t, status.Waiting, "the doctor's queue must show the patient they are about to admit")

	admitted, err := svc.Admit(ctx, doctor, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, admitted.Status)

	// And the doctor's earlier join is still on the participant list: nothing
	// about the refused admit undid it.
	participants, err := st.ListParticipants(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	var sawDoctor bool
	for _, p := range participants {
		if p.Role == RoleDoctor {
			sawDoctor = true
		}
	}
	require.True(t, sawDoctor, "the doctor's early join was lost")
}

// --- an admin role is only an admin role from the admin issuer -------------

// End and Get both accept "not a party, but an admin" as authorisation. Until
// SetAdminIssuer was called at boot, adminIssuer was "" in this process, so
// checkAdminIssuer short-circuited and the role claim was taken at face value.
// user-service signs every patient token and can mint an entirely honest one
// -- correct issuer, correct key, verifying cleanly -- asserting
// realm_access.roles = ["super_admin"]. That token could terminate any live
// consultation on the platform and read back its recording_url.
//
// Reverting Principal.IsAdmin to a bare HasAnyRole(AdminRoles...), or removing
// the middleware.SetAdminIssuer call in cmd/server/main.go, makes this fail.
func TestForgedAdminCannotEndOrReadAConsultation(t *testing.T) {
	const (
		keycloak      = "https://auth.yourapp.lk/realms/telemedicine"
		patientIssuer = "telemed-user-service"
	)
	middleware.SetAdminIssuer(keycloak)
	t.Cleanup(func() { middleware.SetAdminIssuer("") })

	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	for _, role := range middleware.AdminRoles {
		forged := middleware.Principal{
			UserID: uuid.New(),
			Roles:  []middleware.Role{role},
			Issuer: patientIssuer,
		}

		_, _, err := svc.Get(ctx, forged, c.ID)
		require.Error(t, err,
			"role %q asserted by the patient issuer read a consultation it is not a party to", role)

		_, err = svc.End(ctx, forged, c.ID, "admin_ended")
		require.Error(t, err,
			"role %q asserted by the patient issuer ended a live consultation it is not a party to", role)
	}

	// The legitimate path still works: a genuine Keycloak admin can end it.
	genuine := middleware.Principal{
		UserID: uuid.New(),
		Roles:  []middleware.Role{middleware.RoleSuperAdmin},
		Issuer: keycloak,
	}
	_, err = svc.End(ctx, genuine, c.ID, "admin_ended")
	require.NoError(t, err, "a genuine Keycloak super_admin must still be able to end a consultation")
}
