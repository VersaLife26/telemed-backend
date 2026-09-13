package scheduling

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
)

// Advisory-lock keys. Every replica runs the same cron schedule; the lock is
// what makes exactly one of them do the work. Postgres advisory locks are the
// right primitive here rather than a Redis lock: they are released
// automatically when the connection dies, so a pod that is OOM-killed
// mid-generation does not wedge the job until a TTL expires.
const (
	jobLockGenerate    int64 = 0x5C4ED0_01
	jobLockArchive     int64 = 0x5C4ED0_02
	jobLockPartitions  int64 = 0x5C4ED0_03
	jobLockWaitlist    int64 = 0x5C4ED0_04
	jobLockUnpaid      int64 = 0x5C4ED0_05
	jobLockNoShow      int64 = 0x5C4ED0_06
	jobLockOverbooking int64 = 0x5C4ED0_07
	jobLockInvariants  int64 = 0x5C4ED0_08
	jobLockReschedule  int64 = 0x5C4ED0_09
)

// NoShowGrace is how long after a consultation should have ended before an
// appointment nobody closed out is recorded as a no-show.
//
// It is generous on purpose. A patient who never joins is closed earlier by
// consultation-service's late-join sweep (10 minutes after scheduled_at).
// This job is only the backstop for when that never arrives -- a started
// visit the doctor forgot to close, or a confirmed booking with no
// consultation row -- and a wrongly recorded no-show feeds the prepayment
// rule. Twelve hours means a consultation that ran late does not become an
// accusation.
const NoShowGrace = 12 * time.Hour

// Scheduler owns the cron jobs. It is constructed at boot and stopped on
// shutdown alongside the HTTP server.
type Scheduler struct {
	svc      *Service
	lockPool *pgxpool.Pool
	cron     *cron.Cron
	log      zerolog.Logger
	jobMu    sync.Mutex
}

// NewScheduler wires the jobs to the business timezone. Every schedule below is
// expressed in that zone, which is why the daily generation run really does
// happen at midnight in Colombo rather than at 18:30 the previous day.
func NewScheduler(svc *Service, lockPool *pgxpool.Pool, loc *time.Location, log zerolog.Logger) *Scheduler {
	c := cron.New(cron.WithLocation(loc), cron.WithChain(
		// A generation run that overruns must not have a second copy started on
		// top of it.
		cron.SkipIfStillRunning(cronLogger{log}),
		cron.Recover(cronLogger{log}),
	))
	return &Scheduler{svc: svc, lockPool: lockPool, cron: c, log: log}
}

// Register installs the schedule. Times are staggered so that the midnight
// batch does not contend with itself.
func (s *Scheduler) Register(ctx context.Context) error {
	jobs := []struct {
		name string
		spec string
		key  int64
		run  func(context.Context) error
	}{
		// The documented daily run: 00:00 Asia/Colombo, 30 days ahead.
		{"generate-slots", "0 0 * * *", jobLockGenerate, s.runGenerate},
		// Partitions first thing, before anything tries to insert into a month
		// that does not exist yet.
		{"ensure-partitions", "30 23 * * *", jobLockPartitions, s.runPartitions},
		{"archive-slots", "30 0 * * *", jobLockArchive, s.runArchive},
		{"detect-no-shows", "0 1 * * *", jobLockNoShow, s.runNoShowDetection},
		{"recompute-overbooking", "30 1 * * *", jobLockOverbooking, s.runOverbooking},
		// The two sweepers are the only high-frequency jobs; both are bounded
		// batches and both no-op cheaply when there is nothing to do.
		{"sweep-waitlist-offers", "* * * * *", jobLockWaitlist, s.runWaitlistSweep},
		{"sweep-unpaid-bookings", "*/2 * * * *", jobLockUnpaid, s.runUnpaidSweep},
		{"sweep-expired-reschedules", "* * * * *", jobLockReschedule, s.runRescheduleExpiry},
		{"check-invariants", "*/5 * * * *", jobLockInvariants, s.runInvariantCheck},
	}

	for _, j := range jobs {
		name, key, run := j.name, j.key, j.run
		if _, err := s.cron.AddFunc(j.spec, func() {
			s.withJobLock(ctx, name, key, run)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Start begins the schedule and stops it when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.cron.Start()
	s.log.Info().Int("jobs", len(s.cron.Entries())).Msg("scheduler started")
	go func() {
		<-ctx.Done()
		stopCtx := s.cron.Stop()
		<-stopCtx.Done()
		s.log.Info().Msg("scheduler stopped")
	}()
}

// withJobLock runs fn only if this replica wins the advisory lock.
func (s *Scheduler) withJobLock(ctx context.Context, name string, key int64, fn func(context.Context) error) {
	// Jobs run one at a time. The lock connection is held for the whole job
	// while fn draws its own connections from the same pool, so N concurrent
	// jobs pin N connections before any of them can do work. Several jobs
	// share the */10 minute mark, and with a pool of four that deadlocked the
	// domain permanently: every lock holder waited on a connection only a lock
	// holder could release.
	s.jobMu.Lock()
	defer s.jobMu.Unlock()

	// The advisory lock is session-scoped, so it is taken and released on one
	// dedicated connection. Not a transaction: idle_in_transaction_session_timeout
	// would kill the lock session under any job that outlives it.
	conn, err := s.lockPool.Acquire(ctx)
	if err != nil {
		s.log.Error().Err(err).Str("job", name).Msg("job lock connection failed")
		return
	}
	defer conn.Release()

	ok, err := s.svc.repo.TryJobLock(ctx, conn, key)
	if err != nil {
		s.log.Error().Err(err).Str("job", name).Msg("job lock failed")
		return
	}
	if !ok {
		s.log.Debug().Str("job", name).Msg("job already running on another replica")
		return
	}
	defer func() {
		if err := s.svc.repo.ReleaseJobLock(context.WithoutCancel(ctx), conn, key); err != nil {
			// Returning a connection that still holds the lock would block this
			// job on every replica until the connection's lifetime expires.
			s.log.Warn().Err(err).Str("job", name).Msg("job lock release failed; closing connection")
			_ = conn.Hijack().Close(context.WithoutCancel(ctx))
		}
	}()

	start := time.Now()
	if err := fn(ctx); err != nil {
		s.log.Error().Err(err).Str("job", name).Dur("elapsed", time.Since(start)).Msg("job failed")
		return
	}
	s.log.Info().Str("job", name).Dur("elapsed", time.Since(start)).Msg("job completed")
}

func (s *Scheduler) runGenerate(ctx context.Context) error {
	doctors, inserted, err := s.svc.GenerateAll(ctx)
	if err != nil {
		return err
	}
	s.svc.metrics.SlotsGenerated.Add(float64(inserted))
	s.log.Info().Int("doctors", doctors).Int64("slots", inserted).Msg("daily slot generation")
	return nil
}

func (s *Scheduler) runPartitions(ctx context.Context) error {
	names, err := s.svc.EnsurePartitionRunway(ctx)
	if err != nil {
		return err
	}
	s.log.Info().Int("partitions", len(names)).Msg("slot partition runway ensured")
	return nil
}

func (s *Scheduler) runArchive(ctx context.Context) error {
	n, err := s.svc.ArchiveOldSlots(ctx)
	if err != nil {
		return err
	}
	s.svc.metrics.SlotsArchived.Add(float64(n))
	// Idempotency records outlive the JetStream retention window by a day, then
	// go: keeping them forever would turn a small table into a large one for no
	// benefit, because a redelivery cannot arrive after retention expires.
	if _, err := s.svc.repo.PruneConsumedEvents(ctx, s.svc.pool, s.svc.clock.Now().Add(-8*24*time.Hour)); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) runWaitlistSweep(ctx context.Context) error {
	_, err := s.svc.SweepExpiredOffers(ctx)
	return err
}

func (s *Scheduler) runUnpaidSweep(ctx context.Context) error {
	_, err := s.svc.SweepUnpaidBookings(ctx)
	return err
}

func (s *Scheduler) runRescheduleExpiry(ctx context.Context) error {
	_, err := s.svc.SweepExpiredRescheduleRequests(ctx)
	return err
}

func (s *Scheduler) runNoShowDetection(ctx context.Context) error {
	_, err := s.svc.DetectNoShows(ctx)
	return err
}

func (s *Scheduler) runOverbooking(ctx context.Context) error {
	_, err := s.svc.RecomputeOverbooking(ctx)
	return err
}

func (s *Scheduler) runInvariantCheck(ctx context.Context) error {
	return s.svc.CheckInvariants(ctx)
}

// cronLogger adapts zerolog to robfig/cron's logger interface.
type cronLogger struct{ log zerolog.Logger }

func (c cronLogger) Info(msg string, keysAndValues ...any) {
	c.log.Debug().Fields(kv(keysAndValues)).Msg(msg)
}

func (c cronLogger) Error(err error, msg string, keysAndValues ...any) {
	c.log.Error().Err(err).Fields(kv(keysAndValues)).Msg(msg)
}

func kv(pairs []any) map[string]any {
	out := make(map[string]any, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			out[k] = pairs[i+1]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Job bodies that belong to the service rather than the scheduler
// ---------------------------------------------------------------------------

// SweepUnpaidBookings releases slots held by bookings that never got paid for.
// Without it, a patient who opens checkout and closes the app parks a prime-time
// slot until the consultation is over.
func (s *Service) SweepUnpaidBookings(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cutoff := now.Add(-UnpaidBookingWindow)
	const batch = 200

	stale, err := s.repo.ListStaleAppointments(ctx, s.pool, AppointmentPendingPayment, "created_at", cutoff, batch)
	if err != nil {
		return 0, err
	}

	swept := 0
	for i := range stale {
		a := &stale[i]
		var released bool
		var doctorID, slotID = a.DoctorID, a.SlotID
		err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			var err error
			released, doctorID, slotID, err = s.ReleaseForFailedPayment(ctx, tx, a.ID, "payment_timeout")
			return err
		})
		if err != nil {
			s.log.Error().Err(err).Str("appointment_id", maskID(a.ID)).Msg("unpaid booking sweep failed")
			continue
		}
		swept++
		if released {
			if err := s.PromoteWaitlist(ctx, doctorID, slotID); err != nil {
				s.log.Error().Err(err).Msg("waitlist promotion after unpaid sweep failed")
			}
		}
	}
	if swept > 0 {
		s.log.Info().Int("released", swept).Msg("unpaid bookings swept")
	}
	return swept, nil
}

// DetectNoShows closes out confirmed appointments that nobody reported an
// outcome for. See NoShowGrace for why the window is long.
func (s *Service) DetectNoShows(ctx context.Context) (int, error) {
	now := s.clock.Now()
	cutoff := now.Add(-NoShowGrace)
	const batch = 500

	stale, err := s.repo.ListStaleAppointments(ctx, s.pool, AppointmentConfirmed, "slot_end_at", cutoff, batch)
	if err != nil {
		return 0, err
	}

	marked := 0
	for i := range stale {
		a := &stale[i]
		if _, err := s.MarkTerminal(ctx, a.ID, a.PatientID, "system", AppointmentNoShow); err != nil {
			s.log.Error().Err(err).Str("appointment_id", maskID(a.ID)).Msg("no-show detection failed")
			continue
		}
		marked++
	}
	if marked > 0 {
		s.log.Info().Int("marked", marked).Msg("no-shows recorded by the nightly backstop")
	}
	return marked, nil
}

// RecomputeOverbooking raises daily capacity for doctors whose patients no-show
// heavily, and lowers it back when they stop.
//
// "Overbook by 10%" here means ten percent more appointments in the day, never
// two patients in one time window: a double-booked video consultation is a
// clinical incident, not a revenue optimisation. See docs/DESIGN.md §5.
func (s *Service) RecomputeOverbooking(ctx context.Context) (int, error) {
	const (
		window    = 90 * 24 * time.Hour
		minSample = 20
	)
	rates, err := s.repo.DoctorNoShowRates(ctx, s.pool, s.clock.Now().Add(-window), minSample)
	if err != nil {
		return 0, err
	}

	changed := 0
	for _, r := range rates {
		percent := 0
		if r.Rate > DoctorOverbookingThreshold {
			percent = OverbookingPercent
		}
		rate := r
		err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return s.repo.SetOverbookingPercent(ctx, tx, rate.DoctorID, percent)
		})
		if err != nil {
			s.log.Error().Err(err).Str("doctor_id", maskID(rate.DoctorID)).Msg("overbooking update failed")
			continue
		}
		changed++
	}
	return changed, nil
}

// CheckInvariants runs the double-booking post-condition as a live probe and
// publishes it as a gauge. The same query the concurrency test asserts on runs
// in production every five minutes, which is the difference between a test that
// proves something once and an invariant that stays proved.
func (s *Service) CheckInvariants(ctx context.Context) error {
	doubled, err := s.repo.CountDoubleBookedSlots(ctx, s.pool)
	if err != nil {
		return err
	}
	s.metrics.DoubleBookedSlots.Set(float64(doubled))
	if doubled > 0 {
		s.log.Error().Int64("count", doubled).
			Msg("INVARIANT VIOLATED: slots holding more than one live appointment")
	}

	inconsistent, err := s.repo.CountInconsistentSlots(ctx, s.pool)
	if err != nil {
		return err
	}
	if inconsistent > 0 {
		s.log.Error().Int64("count", inconsistent).
			Msg("INVARIANT VIOLATED: slot booked state disagrees with appointments")
	}
	return nil
}
