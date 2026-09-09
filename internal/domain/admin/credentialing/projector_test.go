package credentialing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/platform/events"
)

// --- fakes ------------------------------------------------------------------

// fakeStore mirrors the two repository writes, including the ordering guard
// the SQL enforces, so the projector's own rules can be exercised without a
// Postgres container.
type fakeStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]DoctorSummary
	docs map[uuid.UUID]time.Time // documents_updated_at
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]DoctorSummary{}, docs: map[uuid.UUID]time.Time{}}
}

func (f *fakeStore) UpsertDoctorFromEvent(_ context.Context, _ uuid.UUID, d DoctorSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirrors the COALESCE(NULLIF(...)) in the real upsert: an empty key from
	// a redelivered registration must never blank one already projected.
	if prev, ok := f.rows[d.DoctorID]; ok {
		keep := func(next, old string) string {
			if next == "" {
				return old
			}
			return next
		}
		d.SLMCCertificateKey = keep(d.SLMCCertificateKey, prev.SLMCCertificateKey)
		d.NICDocumentKey = keep(d.NICDocumentKey, prev.NICDocumentKey)
		d.DegreeCertificateKey = keep(d.DegreeCertificateKey, prev.DegreeCertificateKey)
		d.PhotoKey = keep(d.PhotoKey, prev.PhotoKey)
	}
	f.rows[d.DoctorID] = d
	return nil
}

func (f *fakeStore) ApplyDocumentKeys(_ context.Context, id uuid.UUID, d DoctorSummary, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.docs[id]; ok && !at.After(last) {
		return nil // the ordering guard: not strictly newer, so no-op
	}
	row, ok := f.rows[id]
	if !ok {
		// Mirrors "UPDATE ... WHERE doctor_id = $1" matching zero rows: not an
		// error, because the registration event may not have landed yet.
		f.docs[id] = at
		return nil
	}
	row.SLMCCertificateKey = d.SLMCCertificateKey
	row.NICDocumentKey = d.NICDocumentKey
	row.DegreeCertificateKey = d.DegreeCertificateKey
	row.PhotoKey = d.PhotoKey
	f.rows[id] = row
	f.docs[id] = at
	return nil
}

func (f *fakeStore) SetVerificationStatus(_ context.Context, id uuid.UUID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return nil
	}
	row.VerificationStatus = status
	f.rows[id] = row
	return nil
}

func (f *fakeStore) get(id uuid.UUID) DoctorSummary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

type fakeDirectory struct {
	user directory.User
	err  error
}

func (f fakeDirectory) User(context.Context, uuid.UUID) (directory.User, error) {
	return f.user, f.err
}

func envelopeFor(t *testing.T, subject events.Subject, payload any) events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(subject, "telemed-doctor-service", "", payload)
	require.NoError(t, err)
	return env
}

func newTestProjector(dir directory.Client) (*Projector, *fakeStore) {
	st := newFakeStore()
	return NewProjector(st, dir, zerolog.Nop()), st
}

// --- tests ------------------------------------------------------------------

// TestProjector_RegisteredFillsEveryQueueColumn is the regression test for the
// blank credentialing queue. Live, the API returned
// {"full_name":"","email":"","years_experience":0,"registered_at":"0001-01-01"}
// because the producer sent four fields and this consumer declared eleven --
// and encoding/json does not error on a field nobody sent.
func TestProjector_RegisteredFillsEveryQueueColumn(t *testing.T) {
	dir := fakeDirectory{user: directory.User{
		FullName: "Ignored Directory Name", Email: "anula@example.lk", Phone: "+94771234567",
	}}
	p, st := newTestProjector(dir)

	doctorID, userID := uuid.New(), uuid.New()
	registeredAt := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)

	require.NoError(t, p.handle(context.Background(), envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{
			DoctorID:        doctorID,
			UserID:          userID,
			FullName:        "Dr Anula Perera",
			SLMCNumber:      "SLMC/12345",
			Specialty:       "cardiology",
			YearsExperience: 12,
			CreatedAt:       registeredAt,
		})))

	got := st.get(doctorID)
	require.Equal(t, doctorID, got.DoctorID)
	require.Equal(t, "Dr Anula Perera", got.FullName,
		"the name on the APPLICATION wins; it must not shift under a reviewer because the user record was edited")
	require.Equal(t, "SLMC/12345", got.SLMCNumber)
	require.Equal(t, "cardiology", got.SpecialtyCode)
	require.Equal(t, 12, got.YearsExperience)
	require.Equal(t, registeredAt, got.RegisteredAt)
	require.False(t, got.RegisteredAt.IsZero(), "0001-01-01 is what the queue showed before")

	// Contact details still come from user-service, not the event.
	require.Equal(t, "anula@example.lk", got.Email)
	require.Equal(t, "+94771234567", got.Phone)
}

// TestProjector_RegisteredFallsBackToDirectoryNameOnlyWhenTheEventHasNone
// pins the precedence, because getting it backwards is silent.
func TestProjector_RegisteredFallsBackToDirectoryNameOnlyWhenTheEventHasNone(t *testing.T) {
	dir := fakeDirectory{user: directory.User{FullName: "Anula Perera", Email: "a@example.lk"}}
	p, st := newTestProjector(dir)

	doctorID := uuid.New()
	require.NoError(t, p.handle(context.Background(), envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{DoctorID: doctorID, UserID: uuid.New(), SLMCNumber: "SLMC/9"})))

	require.Equal(t, "Anula Perera", st.get(doctorID).FullName,
		"a producer predating full_name must still project a nameless-free row")
}

// TestProjector_DocumentsUpdatedGivesTheReviewerSomethingToOpen is the
// regression test for Gap 3: without doctor.documents_updated the queue shows
// every application with an empty document viewer, because registration always
// precedes the uploads.
func TestProjector_DocumentsUpdatedGivesTheReviewerSomethingToOpen(t *testing.T) {
	p, st := newTestProjector(fakeDirectory{})
	ctx := context.Background()
	doctorID, userID := uuid.New(), uuid.New()

	// 1. The doctor registers. No credential has been uploaded yet, which is
	//    the normal, correct state at this instant.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{
			DoctorID: doctorID, UserID: userID, FullName: "Dr Anula Perera",
			SLMCNumber: "SLMC/12345", CreatedAt: time.Now().UTC(),
		})))
	require.True(t, st.get(doctorID).Empty(), "nothing uploaded yet")

	// 2. The credentials are uploaded, one at a time. Each event carries the
	//    FULL current set, so a consumer that missed one converges on the next.
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated,
		events.DoctorDocumentsUpdated{
			DoctorID: doctorID, UserID: userID,
			DoctorCredentialDocuments: events.DoctorCredentialDocuments{
				SLMCCertificateKey: "doctors/slmc.pdf",
			},
			UpdatedAt: at,
		})))
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated,
		events.DoctorDocumentsUpdated{
			DoctorID: doctorID, UserID: userID,
			DoctorCredentialDocuments: events.DoctorCredentialDocuments{
				SLMCCertificateKey:   "doctors/slmc.pdf",
				NICDocumentKey:       "doctors/nic.jpg",
				DegreeCertificateKey: "doctors/degree.pdf",
				PhotoKey:             "doctors/photo.jpg",
			},
			UpdatedAt: at.Add(time.Minute),
		})))

	got := st.get(doctorID)
	require.False(t, got.Empty(), "the reviewer must have documents to open")
	require.Equal(t, "doctors/slmc.pdf", got.SLMCCertificateKey)
	require.Equal(t, "doctors/nic.jpg", got.NICDocumentKey)
	require.Equal(t, "doctors/degree.pdf", got.DegreeCertificateKey)
	require.Equal(t, "doctors/photo.jpg", got.PhotoKey)
	// And the application details survived the document writes.
	require.Equal(t, "Dr Anula Perera", got.FullName)
	require.Equal(t, "SLMC/12345", got.SLMCNumber)
}

// TestProjector_DuplicateAndOutOfOrderDeliveriesDoNotRegress covers the
// at-least-once contract: JetStream guarantees redelivery, not order.
func TestProjector_DuplicateAndOutOfOrderDeliveriesDoNotRegress(t *testing.T) {
	p, st := newTestProjector(fakeDirectory{})
	ctx := context.Background()
	doctorID, userID := uuid.New(), uuid.New()
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{DoctorID: doctorID, UserID: userID, FullName: "Dr A", CreatedAt: at})))

	full := events.DoctorDocumentsUpdated{
		DoctorID: doctorID, UserID: userID,
		DoctorCredentialDocuments: events.DoctorCredentialDocuments{
			SLMCCertificateKey: "slmc.pdf", NICDocumentKey: "nic.jpg",
			DegreeCertificateKey: "degree.pdf", PhotoKey: "photo.jpg",
		},
		UpdatedAt: at.Add(2 * time.Minute),
	}
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated, full)))

	// Redelivery of the same event: a distinct envelope id, same payload.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated, full)))
	require.Equal(t, "nic.jpg", st.get(doctorID).NICDocumentKey)

	// An OLDER documents event arriving late must not roll the set back to
	// the single document it knew about.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated,
		events.DoctorDocumentsUpdated{
			DoctorID: doctorID, UserID: userID,
			DoctorCredentialDocuments: events.DoctorCredentialDocuments{SLMCCertificateKey: "slmc.pdf"},
			UpdatedAt:                 at.Add(time.Minute),
		})))
	got := st.get(doctorID)
	require.Equal(t, "nic.jpg", got.NICDocumentKey, "an out-of-order delivery rolled the reviewer's documents back")
	require.Equal(t, "photo.jpg", got.PhotoKey)

	// And a redelivered REGISTRATION, whose key set is empty by construction,
	// must not blank the documents either.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{DoctorID: doctorID, UserID: userID, FullName: "Dr A", CreatedAt: at})))
	require.False(t, st.get(doctorID).Empty(),
		"a replayed doctor.registered must not erase credentials uploaded after it")
}

// TestProjector_DocumentsBeforeRegistrationIsNotAnError: the two subjects have
// independent consumers, so the upload can genuinely win the race. Failing
// here would spin the consumer on a redelivery loop that cannot succeed.
func TestProjector_DocumentsBeforeRegistrationIsNotAnError(t *testing.T) {
	p, _ := newTestProjector(fakeDirectory{})
	err := p.handle(context.Background(), envelopeFor(t, events.SubjectDoctorDocumentsUpdated,
		events.DoctorDocumentsUpdated{
			DoctorID: uuid.New(), UserID: uuid.New(),
			DoctorCredentialDocuments: events.DoctorCredentialDocuments{NICDocumentKey: "nic.jpg"},
			UpdatedAt:                 time.Now().UTC(),
		}))
	require.NoError(t, err)
}

// TestProjector_MissingUpdatedAtFallsBackToTheEnvelope keeps the ordering
// guard guarding when a producer omits the field: an all-zero timestamp would
// make every delivery look simultaneous and silently disable it.
func TestProjector_MissingUpdatedAtFallsBackToTheEnvelope(t *testing.T) {
	p, st := newTestProjector(fakeDirectory{})
	ctx := context.Background()
	doctorID := uuid.New()

	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{DoctorID: doctorID, UserID: uuid.New(), CreatedAt: time.Now().UTC()})))

	env := envelopeFor(t, events.SubjectDoctorDocumentsUpdated, events.DoctorDocumentsUpdated{
		DoctorID:                  doctorID,
		DoctorCredentialDocuments: events.DoctorCredentialDocuments{NICDocumentKey: "nic.jpg"},
		// UpdatedAt deliberately zero.
	})
	require.NoError(t, p.handle(ctx, env))
	require.Equal(t, "nic.jpg", st.get(doctorID).NICDocumentKey)

	// A later event still wins, which is what proves the guard is live rather
	// than pinned at the zero time.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectDoctorDocumentsUpdated,
		events.DoctorDocumentsUpdated{
			DoctorID: doctorID,
			DoctorCredentialDocuments: events.DoctorCredentialDocuments{
				NICDocumentKey: "nic-2.jpg", PhotoKey: "photo.jpg",
			},
			UpdatedAt: env.OccurredAt.Add(time.Minute),
		})))
	require.Equal(t, "nic-2.jpg", st.get(doctorID).NICDocumentKey)
}

// TestProjector_UnreachableDirectoryRetriesRatherThanProjectingNameless: a
// nameless row is the bug this projector was rewritten to fix, so an
// unreachable user-service must surface as a redelivery, not as a blank.
func TestProjector_UnreachableDirectoryRetriesRatherThanProjectingNameless(t *testing.T) {
	p, st := newTestProjector(fakeDirectory{err: context.DeadlineExceeded})
	doctorID := uuid.New()
	err := p.handle(context.Background(), envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{DoctorID: doctorID, UserID: uuid.New(), CreatedAt: time.Now().UTC()}))
	require.Error(t, err)
	require.Equal(t, uuid.Nil, st.get(doctorID).DoctorID, "nothing should have been projected")
}

// TestProjector_UnknownUserStillProjects: a doctor missing from the queue is
// worse than a doctor shown without contact details.
func TestProjector_UnknownUserStillProjects(t *testing.T) {
	p, st := newTestProjector(fakeDirectory{err: directory.ErrNotFound})
	doctorID := uuid.New()
	require.NoError(t, p.handle(context.Background(), envelopeFor(t, events.SubjectDoctorRegistered,
		events.DoctorRegistered{
			DoctorID: doctorID, UserID: uuid.New(), FullName: "Dr A",
			SLMCNumber: "SLMC/1", CreatedAt: time.Now().UTC(),
		})))
	require.Equal(t, "Dr A", st.get(doctorID).FullName)
	require.Empty(t, st.get(doctorID).Email)
}

// TestDoctorRegisteredWireCompatibility decodes the exact bytes
// doctor-service's golden test pins, so a drift on either side is caught in
// this repo too -- the two services cannot import each other's tests.
func TestDoctorRegisteredWireCompatibility(t *testing.T) {
	const goldenFromDoctorService = `{` +
		`"doctor_id":"11111111-1111-4111-8111-111111111111",` +
		`"user_id":"22222222-2222-4222-8222-222222222222",` +
		`"full_name":"Dr Anula Perera",` +
		`"slmc_number":"SLMC/12345",` +
		`"specialty":"cardiology",` +
		`"years_experience":12,` +
		`"slmc_certificate_key":"doctors/11111111/slmc.pdf",` +
		`"nic_document_key":"doctors/11111111/nic.jpg",` +
		`"degree_certificate_key":"doctors/11111111/degree.pdf",` +
		`"photo_key":"doctors/11111111/photo.jpg",` +
		`"created_at":"2026-08-20T09:30:00Z"` +
		`}`

	var got events.DoctorRegistered
	require.NoError(t, json.Unmarshal([]byte(goldenFromDoctorService), &got))

	require.Equal(t, "Dr Anula Perera", got.FullName)
	require.Equal(t, 12, got.YearsExperience)
	require.Equal(t, "doctors/11111111/slmc.pdf", got.SLMCCertificateKey)
	require.Equal(t, "doctors/11111111/nic.jpg", got.NICDocumentKey)
	require.Equal(t, "doctors/11111111/degree.pdf", got.DegreeCertificateKey)
	require.Equal(t, "doctors/11111111/photo.jpg", got.PhotoKey)
	require.Equal(t, time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), got.CreatedAt)
}
