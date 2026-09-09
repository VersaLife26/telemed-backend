//go:build integration

package credentialing_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/credentialing"
	"telemed/internal/domain/admin/testutil"
)

// These exercise the SQL the unit tests can only mirror: the
// COALESCE(NULLIF(...)) that stops a replayed registration from blanking a
// reviewer's documents, and the documents_updated_at ordering guard that stops
// an out-of-order redelivery from rolling them back. Both are the kind of
// clause that is easy to write, easy to get subtly wrong, and impossible to
// notice going wrong from the outside -- the queue just quietly shows an empty
// document viewer again.

func summary(doctorID uuid.UUID) credentialing.DoctorSummary {
	return credentialing.DoctorSummary{
		DoctorID:        doctorID,
		FullName:        "Dr Anula Perera",
		Email:           "anula@example.lk",
		Phone:           "+94770000101",
		SLMCNumber:      "SLMC/" + doctorID.String()[:8],
		YearsExperience: 12,
		SpecialtyCode:   "cardiology",
		RegisteredAt:    time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC),
	}
}

func TestIntegration_DocumentKeysSurviveAReplayedRegistration(t *testing.T) {
	ctx := context.Background()
	repo := credentialing.NewRepository(testutil.StartPostgres(t))
	doctorID := uuid.New()

	// 1. The application arrives. No credential has been uploaded yet, which is
	//    the normal state at this instant: registration precedes every upload.
	require.NoError(t, repo.UpsertDoctorFromEvent(ctx, uuid.New(), summary(doctorID)))

	got, err := repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.True(t, got.Empty())
	require.Equal(t, "Dr Anula Perera", got.FullName)
	require.Equal(t, 12, got.YearsExperience)
	require.False(t, got.RegisteredAt.IsZero(),
		"a zero registered_at is what made the queue unorderable")

	// 2. The credentials are uploaded.
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	full := credentialing.DoctorSummary{
		SLMCCertificateKey:   "slmc.pdf",
		NICDocumentKey:       "nic.jpg",
		DegreeCertificateKey: "degree.pdf",
		PhotoKey:             "photo.jpg",
	}
	require.NoError(t, repo.ApplyDocumentKeys(ctx, doctorID, full, at))

	got, err = repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.Equal(t, "nic.jpg", got.NICDocumentKey)
	require.Equal(t, "photo.jpg", got.PhotoKey)

	// 3. JetStream redelivers the REGISTRATION -- a different event id, an
	//    empty key set. It must not erase the documents.
	require.NoError(t, repo.UpsertDoctorFromEvent(ctx, uuid.New(), summary(doctorID)))

	got, err = repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.False(t, got.Empty(),
		"a replayed doctor.registered blanked the reviewer's documents")
	require.Equal(t, "slmc.pdf", got.SLMCCertificateKey)
	require.Equal(t, "degree.pdf", got.DegreeCertificateKey)
}

func TestIntegration_DocumentKeyOrderingGuard(t *testing.T) {
	ctx := context.Background()
	repo := credentialing.NewRepository(testutil.StartPostgres(t))
	doctorID := uuid.New()
	require.NoError(t, repo.UpsertDoctorFromEvent(ctx, uuid.New(), summary(doctorID)))

	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	full := credentialing.DoctorSummary{
		SLMCCertificateKey:   "slmc.pdf",
		NICDocumentKey:       "nic.jpg",
		DegreeCertificateKey: "degree.pdf",
		PhotoKey:             "photo.jpg",
	}
	require.NoError(t, repo.ApplyDocumentKeys(ctx, doctorID, full, at))

	// An OLDER event arriving late -- it knew about one document only.
	partial := credentialing.DoctorSummary{SLMCCertificateKey: "slmc.pdf"}
	require.NoError(t, repo.ApplyDocumentKeys(ctx, doctorID, partial, at.Add(-time.Minute)))

	got, err := repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.Equal(t, "nic.jpg", got.NICDocumentKey,
		"an out-of-order redelivery rolled the document set backwards")

	// A duplicate of the newest event is a no-op, not a second write.
	require.NoError(t, repo.ApplyDocumentKeys(ctx, doctorID, partial, at))
	got, err = repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.Equal(t, "photo.jpg", got.PhotoKey)

	// A genuinely newer event does apply: the guard must guard, not freeze.
	replaced := credentialing.DoctorSummary{
		SLMCCertificateKey:   "slmc.pdf",
		NICDocumentKey:       "nic-v2.jpg",
		DegreeCertificateKey: "degree.pdf",
		PhotoKey:             "photo.jpg",
	}
	require.NoError(t, repo.ApplyDocumentKeys(ctx, doctorID, replaced, at.Add(time.Minute)))
	got, err = repo.GetDoctor(ctx, doctorID)
	require.NoError(t, err)
	require.Equal(t, "nic-v2.jpg", got.NICDocumentKey,
		"a re-upload after a rejection must be what the reviewer opens next")
}

// TestIntegration_DocumentsBeforeRegistrationIsNotAnError: the two subjects
// have independent consumers, so the upload event can win the race. Returning
// an error would spin the consumer on a redelivery loop that cannot succeed.
func TestIntegration_DocumentsBeforeRegistrationIsNotAnError(t *testing.T) {
	ctx := context.Background()
	repo := credentialing.NewRepository(testutil.StartPostgres(t))

	err := repo.ApplyDocumentKeys(ctx, uuid.New(),
		credentialing.DoctorSummary{NICDocumentKey: "nic.jpg"}, time.Now().UTC())
	require.NoError(t, err)
}
