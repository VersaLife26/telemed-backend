//go:build integration

package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/analytics"
	"telemed/internal/domain/admin/testutil"
)

// TestRefresher_MaterializedViewsReflectProjectedData is the test the brief
// calls out explicitly: insert into the event-fed projection tables, refresh,
// and confirm the materialized views actually pick up the new rows -- and
// that REFRESH MATERIALIZED VIEW CONCURRENTLY does not error, which requires
// the unique index every view was created with in
// migrations/000004_analytics.up.sql.
func TestRefresher_MaterializedViewsReflectProjectedData(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := analytics.NewRepository(pool)
	refresher := analytics.NewRefresher(repo, zerolog.Nop())

	// Before any data and any refresh, the views exist but are empty.
	empty, err := repo.Revenue(ctx, analytics.DateRange{})
	require.NoError(t, err)
	require.Empty(t, empty)

	day := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	doctorID := uuid.New()

	require.NoError(t, repo.UpsertPayment(ctx, uuid.New(), "succeeded", analytics.PaymentFact{
		PaymentID: uuid.New(), DoctorID: doctorID, SpecialtyCode: "cardiology", District: "Colombo",
		AmountCents: 250000, CommissionCents: 50000, Currency: "LKR", Provider: "stripe", OccurredAt: day,
	}))
	require.NoError(t, repo.UpsertPayment(ctx, uuid.New(), "succeeded", analytics.PaymentFact{
		PaymentID: uuid.New(), DoctorID: doctorID, SpecialtyCode: "cardiology", District: "Colombo",
		AmountCents: 150000, CommissionCents: 30000, Currency: "LKR", Provider: "payhere", OccurredAt: day.Add(time.Hour),
	}))
	require.NoError(t, repo.UpsertAppointment(ctx, uuid.New(), "completed", analytics.AppointmentFact{
		AppointmentID: uuid.New(), DoctorID: doctorID, SpecialtyCode: "cardiology", District: "Colombo",
		ScheduledAt: day, OccurredAt: day,
	}))
	require.NoError(t, repo.UpsertAppointment(ctx, uuid.New(), "no_show", analytics.AppointmentFact{
		AppointmentID: uuid.New(), DoctorID: doctorID, SpecialtyCode: "cardiology", District: "Gampaha",
		ScheduledAt: day, OccurredAt: day,
	}))

	// Immediately after the projection write, the (unrefreshed) view must
	// still be empty -- this is the property that makes CONCURRENTLY refresh
	// meaningful rather than accidental.
	stillEmpty, err := repo.Revenue(ctx, analytics.DateRange{})
	require.NoError(t, err)
	require.Empty(t, stillEmpty, "materialized view must not reflect writes before a refresh runs")

	require.NoError(t, refresher.RunOnce(ctx))

	revenue, err := repo.Revenue(ctx, analytics.DateRange{})
	require.NoError(t, err)
	require.Len(t, revenue, 1)
	require.Equal(t, int64(400000), revenue[0].GrossCents)
	require.Equal(t, int64(80000), revenue[0].CommissionCents)
	require.EqualValues(t, 2, revenue[0].PaymentCount)

	districts, err := repo.Districts(ctx, analytics.DateRange{})
	require.NoError(t, err)
	require.Len(t, districts, 2, "Colombo and Gampaha")

	topDoctors, err := repo.TopDoctors(ctx, analytics.DateRange{}, 10)
	require.NoError(t, err)
	require.Len(t, topDoctors, 1)
	require.Equal(t, doctorID, topDoctors[0].DoctorID)
	require.EqualValues(t, 1, topDoctors[0].CompletedCount)
	require.EqualValues(t, 1, topDoctors[0].NoShowCount)

	// Refreshing again with no new data must still succeed (this is exactly
	// what CONCURRENTLY needs the unique index for: a refresh against
	// unchanged source data still has to diff old vs new rows).
	require.NoError(t, refresher.RunOnce(ctx))
}
