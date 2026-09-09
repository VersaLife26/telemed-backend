//go:build integration

package consultation_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// The postgres (lib/pq) driver, not the native pgx pool this service uses
	// at runtime: golang-migrate needs a database/sql driver to run schema
	// migrations, and pulling in lib/pq for this one test file is simpler
	// than wiring pgx's database/sql adapter just for a migration runner.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"telemed/internal/domain/consultation/consultation"
	"telemed/internal/platform/database"
	"telemed/internal/platform/repopath"
)

// These tests exercise Repository against a real Postgres 17 via
// testcontainers, the same image AGENT-BRIEF.md pins for the platform. They
// are NOT run as part of `go test ./...` -- only `go test -tags=integration`
// -- because they need a working Docker daemon, and this development
// machine's Docker/disk were deliberately left alone during the main
// verification pass (see the service's README and the build report). The
// code is believed correct; it has not been executed against a live
// container by the agent that wrote it.
func TestRepository_Integration(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("telemed_consultation"),
		postgres.WithUsername("telemed"),
		postgres.WithPassword("telemed"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	applyMigrations(t, dsn)

	pool, err := database.Connect(ctx, database.Config{URL: dsn, MaxConns: 5}, zerolog.Nop())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repo := consultation.NewRepository()

	t.Run("create and duplicate detection", func(t *testing.T) {
		c := &consultation.Consultation{
			AppointmentID: uuid.New(),
			PatientID:     uuid.New(),
			DoctorID:      uuid.New(),
			RoomName:      "room-" + uuid.NewString(),
			Status:        consultation.StatusScheduled,
			ScheduledAt:   time.Now().UTC(),
		}
		require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.CreateConsultation(ctx, tx, c)
		}))
		require.NotZero(t, c.ID)
		require.Zero(t, c.Version)

		dup := &consultation.Consultation{
			AppointmentID: c.AppointmentID, // same appointment -> unique violation
			PatientID:     uuid.New(),
			DoctorID:      uuid.New(),
			RoomName:      "room-" + uuid.NewString(),
			Status:        consultation.StatusScheduled,
			ScheduledAt:   time.Now().UTC(),
		}
		err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.CreateConsultation(ctx, tx, dup)
		})
		require.ErrorIs(t, err, consultation.ErrDuplicateConsultation)

		fetched, err := repo.GetConsultation(ctx, pool, c.ID)
		require.NoError(t, err)
		require.Equal(t, c.RoomName, fetched.RoomName)

		byAppt, err := repo.GetConsultationByAppointment(ctx, pool, c.AppointmentID)
		require.NoError(t, err)
		require.Equal(t, c.ID, byAppt.ID)
	})

	t.Run("optimistic lock rejects a stale version", func(t *testing.T) {
		c := &consultation.Consultation{
			AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
			RoomName: "room-" + uuid.NewString(), Status: consultation.StatusScheduled, ScheduledAt: time.Now().UTC(),
		}
		require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.CreateConsultation(ctx, tx, c)
		}))

		stale := *c // a second in-memory copy of the same row, both starting at version 0
		c.Status = consultation.StatusWaiting
		require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.UpdateConsultation(ctx, tx, c)
		}))
		require.Equal(t, 1, c.Version)

		stale.Status = consultation.StatusActive
		err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.UpdateConsultation(ctx, tx, &stale)
		})
		require.ErrorIs(t, err, consultation.ErrOptimisticLock)
	})

	t.Run("events round-trip JSONB metadata", func(t *testing.T) {
		c := &consultation.Consultation{
			AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
			RoomName: "room-" + uuid.NewString(), Status: consultation.StatusScheduled, ScheduledAt: time.Now().UTC(),
		}
		require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return repo.CreateConsultation(ctx, tx, c)
		}))

		identity := c.PatientID.String()
		for range 3 {
			require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return repo.RecordEvent(ctx, tx, consultation.Event{
					ConsultationID: c.ID, Type: consultation.EventQualitySample, ActorIdentity: identity,
					Metadata: map[string]any{"quality": "poor"}, OccurredAt: time.Now().UTC(),
				})
			}))
		}

		recent, err := repo.RecentQualityEvents(ctx, pool, c.ID, identity, 3)
		require.NoError(t, err)
		require.Len(t, recent, 3)
		for _, e := range recent {
			require.Equal(t, "poor", e.Metadata["quality"])
		}
	})

	t.Run("waiting room queue ordering", func(t *testing.T) {
		doctorID := uuid.New()
		var ids []uuid.UUID
		for range 3 {
			c := &consultation.Consultation{
				AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: doctorID,
				RoomName: "room-" + uuid.NewString(), Status: consultation.StatusScheduled, ScheduledAt: time.Now().UTC(),
			}
			require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return repo.CreateConsultation(ctx, tx, c)
			}))
			require.NoError(t, database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return repo.UpsertWaitingRoomEntry(ctx, tx, consultation.WaitingRoomEntry{
					ConsultationID: c.ID, DoctorID: doctorID, PatientID: c.PatientID, EnteredAt: time.Now().UTC(),
				})
			}))
			ids = append(ids, c.ID)
			time.Sleep(2 * time.Millisecond)
		}

		entry, err := repo.GetWaitingRoomEntry(ctx, pool, ids[2])
		require.NoError(t, err)
		ahead, err := repo.CountWaitingAhead(ctx, pool, doctorID, entry.EnteredAt)
		require.NoError(t, err)
		require.EqualValues(t, 2, ahead)
	})

	t.Run("webhook receipt claim is exactly-once", func(t *testing.T) {
		claimed1, err := claimInTx(ctx, pool, repo, "evt-integration-1", "room_finished")
		require.NoError(t, err)
		require.True(t, claimed1)

		claimed2, err := claimInTx(ctx, pool, repo, "evt-integration-1", "room_finished")
		require.NoError(t, err)
		require.False(t, claimed2)
	})
}

func claimInTx(ctx context.Context, pool *pgxpool.Pool, repo *consultation.Repository, eventID, eventType string) (bool, error) {
	var claimed bool
	err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		claimed, err = repo.ClaimWebhookEvent(ctx, tx, eventID, eventType)
		return err
	})
	return claimed, err
}

// applyMigrations runs every migrations/*.up.sql file against dsn using the
// same golang-migrate driver the Makefile's migrate-up target uses, so this
// test exercises the exact schema the service ships, not a hand-copied one.
func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	dir := migrationsDir(t)

	m, err := migrate.New("file://"+dir, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		require.NoError(t, err)
	}
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := repopath.Migrations(t, "consultation")
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "migrations directory must exist at %s", abs)
	return abs
}
