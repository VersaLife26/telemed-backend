package scheduling_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/database"
)

func colombo(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo (is tzdata installed?): %v", err)
	}
	return loc
}

func settings15x5() scheduling.ScheduleSettings {
	s := scheduling.DefaultScheduleSettings(uuid.New())
	s.SlotDurationMinutes = 15
	s.BufferMinutes = 5
	s.MaxPerDay = 100
	return s
}

func hours(day time.Weekday, startMin, endMin int) []scheduling.WorkingHour {
	return []scheduling.WorkingHour{{DayOfWeek: day, StartMinute: startMin, EndMinute: endMin, IsAvailable: true}}
}

// TestSlotGeneration_TimezoneOffset is the test that would catch an offset bug.
//
// A doctor's 09:00 in Colombo is 03:30 UTC. An implementation that built the
// instant in UTC, or that hardcoded an offset, or that ran on a box whose
// TZ happened to be something else, would produce a different instant -- and
// every patient in Sri Lanka would see their morning appointments five and a
// half hours out. This test pins the conversion in both directions.
func TestSlotGeneration_TimezoneOffset(t *testing.T) {
	loc := colombo(t)
	// 2027-03-15 is a Monday. Sri Lanka observes no DST, so the offset is a
	// flat +05:30 all year -- but it is read from tzdata, not assumed.
	date := scheduling.Date{Year: 2027, Month: time.March, Day: 15}
	if date.Weekday() != time.Monday {
		t.Fatalf("fixture date is a %s, expected Monday", date.Weekday())
	}

	plans := scheduling.PlanDay(settings15x5(), hours(time.Monday, 9*60, 12*60), date, loc)
	if len(plans) == 0 {
		t.Fatal("no slots generated")
	}

	wantFirstUTC := time.Date(2027, time.March, 15, 3, 30, 0, 0, time.UTC)
	if !plans[0].StartAt.Equal(wantFirstUTC) {
		t.Fatalf("first slot starts at %s UTC, want %s (09:00 Asia/Colombo)",
			plans[0].StartAt.Format(time.RFC3339), wantFirstUTC.Format(time.RFC3339))
	}

	// The failure this test exists to catch: building the wall clock in UTC.
	naive := time.Date(2027, time.March, 15, 9, 0, 0, 0, time.UTC)
	if plans[0].StartAt.Equal(naive) {
		t.Fatal("slot was built in UTC instead of the doctor's timezone")
	}
	if delta := naive.Sub(plans[0].StartAt); delta != 5*time.Hour+30*time.Minute {
		t.Fatalf("Colombo offset resolved to %v, want 5h30m", delta)
	}

	// Round-tripping through the location must give the wall clock back.
	if got := plans[0].StartAt.In(loc).Format("15:04"); got != "09:00" {
		t.Fatalf("slot renders as %s in Colombo, want 09:00", got)
	}

	// And it is not hardcoded to Sri Lanka: the same clinic hours in Dubai
	// (+04:00, also DST-free) land four hours before UTC.
	dubai, err := time.LoadLocation("Asia/Dubai")
	if err != nil {
		t.Skipf("Asia/Dubai unavailable: %v", err)
	}
	dubaiPlans := scheduling.PlanDay(settings15x5(), hours(time.Monday, 9*60, 12*60), date, dubai)
	wantDubai := time.Date(2027, time.March, 15, 5, 0, 0, 0, time.UTC)
	if !dubaiPlans[0].StartAt.Equal(wantDubai) {
		t.Fatalf("Dubai 09:00 resolved to %s, want %s", dubaiPlans[0].StartAt, wantDubai)
	}
}

// TestSlotGeneration_BufferArithmetic pins the documentation's worked example:
// 09:00-12:00 with a 15 minute slot and a 5 minute buffer.
func TestSlotGeneration_BufferArithmetic(t *testing.T) {
	loc := colombo(t)
	date := scheduling.Date{Year: 2027, Month: time.March, Day: 15}

	cases := []struct {
		name       string
		duration   int
		buffer     int
		startMin   int
		endMin     int
		wantCount  int
		wantStarts []string // local HH:MM, first few
		wantLast   string
	}{
		{
			name: "15m slot, 5m buffer, 3h window", duration: 15, buffer: 5,
			startMin: 9 * 60, endMin: 12 * 60, wantCount: 9,
			wantStarts: []string{"09:00", "09:20", "09:40", "10:00"}, wantLast: "11:40",
		},
		{
			name: "30m slot, no buffer", duration: 30, buffer: 0,
			startMin: 9 * 60, endMin: 12 * 60, wantCount: 6,
			wantStarts: []string{"09:00", "09:30", "10:00"}, wantLast: "11:30",
		},
		{
			// The trailing partial slot must be dropped, not truncated: a
			// 60 minute consultation that would run past close is not offered.
			name: "60m slot into a 2h30m window", duration: 60, buffer: 10,
			startMin: 9 * 60, endMin: 11*60 + 30, wantCount: 2,
			wantStarts: []string{"09:00", "10:10"}, wantLast: "10:10",
		},
		{
			name: "window too short for one slot", duration: 30, buffer: 5,
			startMin: 9 * 60, endMin: 9*60 + 20, wantCount: 0,
		},
		{
			name: "buffer larger than the slot", duration: 10, buffer: 50,
			startMin: 9 * 60, endMin: 12 * 60, wantCount: 3,
			wantStarts: []string{"09:00", "10:00", "11:00"}, wantLast: "11:00",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := settings15x5()
			s.SlotDurationMinutes, s.BufferMinutes = tc.duration, tc.buffer
			plans := scheduling.PlanDay(s, hours(time.Monday, tc.startMin, tc.endMin), date, loc)

			if len(plans) != tc.wantCount {
				got := make([]string, len(plans))
				for i, p := range plans {
					got[i] = p.StartAt.In(loc).Format("15:04")
				}
				t.Fatalf("got %d slots %v, want %d", len(plans), got, tc.wantCount)
			}
			for i, want := range tc.wantStarts {
				if got := plans[i].StartAt.In(loc).Format("15:04"); got != want {
					t.Errorf("slot %d starts at %s, want %s", i, got, want)
				}
			}
			if tc.wantLast != "" {
				last := plans[len(plans)-1]
				if got := last.StartAt.In(loc).Format("15:04"); got != tc.wantLast {
					t.Errorf("last slot starts at %s, want %s", got, tc.wantLast)
				}
				// Every slot is exactly the consultation length -- the buffer
				// is a gap between slots, never billable time inside one.
				if got := last.EndAt.Sub(last.StartAt); got != time.Duration(tc.duration)*time.Minute {
					t.Errorf("slot length %v, want %dm", got, tc.duration)
				}
			}
			// No two slots may overlap, ever.
			for i := 1; i < len(plans); i++ {
				if plans[i].StartAt.Before(plans[i-1].EndAt) {
					t.Fatalf("slot %d starts before slot %d ends", i, i-1)
				}
			}
		})
	}
}

// TestSlotGeneration_MaxPerDayAndOverbooking checks the daily cap and the Mend
// overbooking allowance.
func TestSlotGeneration_MaxPerDayAndOverbooking(t *testing.T) {
	loc := colombo(t)
	date := scheduling.Date{Year: 2027, Month: time.March, Day: 15}
	// 09:00-17:00 with 15+5 is 24 slots.
	week := hours(time.Monday, 9*60, 17*60)

	s := settings15x5()
	s.MaxPerDay = 10
	if got := len(scheduling.PlanDay(s, week, date, loc)); got != 10 {
		t.Fatalf("max_per_day=10 produced %d slots", got)
	}

	s.OverbookingPercent = scheduling.OverbookingPercent // 10%
	if got := s.EffectiveMaxPerDay(); got != 11 {
		t.Fatalf("EffectiveMaxPerDay = %d, want 11 (10 + ceil(10%%))", got)
	}
	if got := len(scheduling.PlanDay(s, week, date, loc)); got != 11 {
		t.Fatalf("overbooked plan produced %d slots, want 11", got)
	}

	// Overbooking adds capacity; it never sells the same minute twice.
	plans := scheduling.PlanDay(s, week, date, loc)
	seen := map[time.Time]bool{}
	for _, p := range plans {
		if seen[p.StartAt] {
			t.Fatalf("overbooking produced two slots starting at %s", p.StartAt)
		}
		seen[p.StartAt] = true
	}
}

// TestSlotGeneration_DSTSafety is a bonus: Sri Lanka is DST-free, but the
// generator must not produce a zero-length or overlapping slot for a doctor who
// is not.
func TestSlotGeneration_DSTSafety(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("America/New_York unavailable: %v", err)
	}
	// 2027-03-14: clocks jump 02:00 -> 03:00. A 01:00-06:00 clinic window
	// straddles the gap.
	date := scheduling.Date{Year: 2027, Month: time.March, Day: 14}
	s := settings15x5()
	s.SlotDurationMinutes, s.BufferMinutes = 60, 0

	plans := scheduling.PlanDay(s, hours(date.Weekday(), 60, 6*60), date, ny)
	for i, p := range plans {
		if !p.EndAt.After(p.StartAt) {
			t.Fatalf("slot %d has zero or negative length: %s..%s", i, p.StartAt, p.EndAt)
		}
		if i > 0 && p.StartAt.Before(plans[i-1].EndAt) {
			t.Fatalf("slot %d overlaps slot %d across the DST boundary", i, i-1)
		}
	}
}

// ---------------------------------------------------------------------------
// Database-backed generation
// ---------------------------------------------------------------------------

func seedDoctorSchedule(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, tz string, duration, buffer, maxPerDay, advanceDays int, wh []scheduling.WorkingHour) {
	t.Helper()
	ctx := context.Background()
	repo := scheduling.NewRepository()
	err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		s := scheduling.DefaultScheduleSettings(doctorID)
		s.Timezone = tz
		s.SlotDurationMinutes, s.BufferMinutes = duration, buffer
		s.MaxPerDay, s.AdvanceDays = maxPerDay, advanceDays
		if err := repo.UpsertScheduleSettings(ctx, tx, s); err != nil {
			return err
		}
		return repo.ReplaceWorkingHours(ctx, tx, doctorID, wh)
	})
	if err != nil {
		t.Fatalf("seed doctor schedule: %v", err)
	}
}

// TestSlotGeneration_IdempotentRerun is the property that lets the nightly cron
// be retried blindly: running it five times inserts each slot exactly once.
func TestSlotGeneration_IdempotentRerun(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID := uuid.New()

	// Available every day, so the run covers a full week regardless of when the
	// test happens to execute.
	var wh []scheduling.WorkingHour
	for d := time.Sunday; d <= time.Saturday; d++ {
		wh = append(wh, scheduling.WorkingHour{DoctorID: doctorID, DayOfWeek: d,
			StartMinute: 9 * 60, EndMinute: 12 * 60, IsAvailable: true})
	}
	seedDoctorSchedule(t, pool, doctorID, "Asia/Colombo", 15, 5, 100, 7, wh)

	first, err := svc.GenerateForDoctor(ctx, doctorID)
	if err != nil {
		t.Fatalf("first generation: %v", err)
	}
	if first.Inserted == 0 {
		t.Fatal("first generation inserted nothing")
	}

	countSlots := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE doctor_id = $1`, doctorID).Scan(&n); err != nil {
			t.Fatalf("count slots: %v", err)
		}
		return n
	}
	after := countSlots()
	if after != first.Inserted {
		t.Fatalf("inserted %d but the table holds %d", first.Inserted, after)
	}

	for i := 0; i < 4; i++ {
		res, err := svc.GenerateForDoctor(ctx, doctorID)
		if err != nil {
			t.Fatalf("rerun %d: %v", i, err)
		}
		if res.Inserted != 0 {
			t.Fatalf("rerun %d inserted %d rows; generation is not idempotent", i, res.Inserted)
		}
	}
	if got := countSlots(); got != after {
		t.Fatalf("slot count drifted from %d to %d across reruns", after, got)
	}

	// Every stored instant must be a real Colombo 09:00-12:00 wall clock.
	loc := colombo(t)
	rows, err := pool.Query(ctx, `SELECT start_at FROM slots WHERE doctor_id = $1 ORDER BY start_at`, doctorID)
	if err != nil {
		t.Fatalf("read slots: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var startAt time.Time
		if err := rows.Scan(&startAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		local := startAt.In(loc)
		if local.Hour() < 9 || local.Hour() >= 12 {
			t.Fatalf("slot at %s falls outside 09:00-12:00 Asia/Colombo", local.Format(time.RFC3339))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
}

// TestSlotGeneration_ExcludesHolidays covers both platform-wide and
// doctor-specific non-working days.
func TestSlotGeneration_ExcludesHolidays(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	repo := scheduling.NewRepository()
	loc := colombo(t)
	doctorID := uuid.New()

	var wh []scheduling.WorkingHour
	for d := time.Sunday; d <= time.Saturday; d++ {
		wh = append(wh, scheduling.WorkingHour{DoctorID: doctorID, DayOfWeek: d,
			StartMinute: 9 * 60, EndMinute: 10 * 60, IsAvailable: true})
	}
	seedDoctorSchedule(t, pool, doctorID, "Asia/Colombo", 30, 0, 100, 7, wh)

	today := scheduling.DateIn(time.Now(), loc)
	platformHoliday := today.AddDays(2) // e.g. a Poya day
	doctorHoliday := today.AddDays(3)

	if err := database.InTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := repo.UpsertHoliday(ctx, tx, nil, platformHoliday, "Poya"); err != nil {
			return err
		}
		_, err := repo.UpsertHoliday(ctx, tx, &doctorID, doctorHoliday, "conference")
		return err
	}); err != nil {
		t.Fatalf("seed holidays: %v", err)
	}

	if _, err := svc.GenerateForDoctor(ctx, doctorID); err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, d := range []scheduling.Date{platformHoliday, doctorHoliday} {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM slots WHERE doctor_id = $1 AND start_at >= $2 AND start_at < $3`,
			doctorID, d.StartOfDay(loc).UTC(), d.EndOfDay(loc).UTC()).Scan(&n)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d slots generated on holiday %s", n, d)
		}
	}

	// A neighbouring, non-holiday day must still have slots, otherwise the test
	// would pass on a generator that produced nothing at all.
	control := today.AddDays(4)
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM slots WHERE doctor_id = $1 AND start_at >= $2 AND start_at < $3`,
		doctorID, control.StartOfDay(loc).UTC(), control.EndOfDay(loc).UTC()).Scan(&n); err != nil {
		t.Fatalf("count control: %v", err)
	}
	if n == 0 {
		t.Fatalf("no slots on the control day %s; the test proves nothing", control)
	}
}

// TestSlotGeneration_NeverInThePast: re-running the job at midday must not
// resurrect this morning's slots.
func TestSlotGeneration_NeverInThePast(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID := uuid.New()

	var wh []scheduling.WorkingHour
	for d := time.Sunday; d <= time.Saturday; d++ {
		wh = append(wh, scheduling.WorkingHour{DoctorID: doctorID, DayOfWeek: d,
			StartMinute: 0, EndMinute: 24 * 60, IsAvailable: true})
	}
	seedDoctorSchedule(t, pool, doctorID, "Asia/Colombo", 30, 0, 200, 2, wh)

	if _, err := svc.GenerateForDoctor(ctx, doctorID); err != nil {
		t.Fatalf("generate: %v", err)
	}

	var earliest time.Time
	if err := pool.QueryRow(ctx,
		`SELECT MIN(start_at) FROM slots WHERE doctor_id = $1`, doctorID).Scan(&earliest); err != nil {
		t.Fatalf("read earliest: %v", err)
	}
	if !earliest.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("generated a slot in the past: %s", earliest)
	}
}

// TestArchiveOldSlots moves stale rows out of the live partitions.
func TestArchiveOldSlots(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID := uuid.New()

	old := seedSlot(t, pool, doctorID, time.Now().Add(-100*24*time.Hour), 15*time.Minute)
	recent := seedSlot(t, pool, doctorID, time.Now().Add(-10*24*time.Hour), 15*time.Minute)

	moved, err := svc.ArchiveOldSlots(ctx)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if moved != 1 {
		t.Fatalf("archived %d slots, want 1", moved)
	}

	var liveOld, liveRecent, archivedOld int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE id = $1`, old).Scan(&liveOld)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE id = $1`, recent).Scan(&liveRecent)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots_archive WHERE id = $1`, old).Scan(&archivedOld)

	if liveOld != 0 || archivedOld != 1 {
		t.Fatalf("the 100-day-old slot was not archived (live=%d archived=%d)", liveOld, archivedOld)
	}
	if liveRecent != 1 {
		t.Fatal("the 10-day-old slot was archived; the 90-day cutoff is wrong")
	}
}

// TestEnsurePartitionRunway proves the partition helper is idempotent and
// actually creates the months it claims to.
func TestEnsurePartitionRunway(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)

	first, err := svc.EnsurePartitionRunway(ctx)
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if len(first) != scheduling.PartitionRunwayMonths {
		t.Fatalf("got %d partitions, want %d", len(first), scheduling.PartitionRunwayMonths)
	}

	second, err := svc.EnsurePartitionRunway(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("second run returned %d names, want %d", len(second), len(first))
	}

	// Each named partition must really be attached to slots.
	for _, name := range first {
		var attached bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_inherits i
				JOIN pg_class c ON c.oid = i.inhrelid
				JOIN pg_class p ON p.oid = i.inhparent
				WHERE p.relname = 'slots' AND c.relname = $1
			)`, name).Scan(&attached)
		if err != nil {
			t.Fatalf("check partition %s: %v", name, err)
		}
		if !attached {
			t.Fatalf("partition %s is not attached to slots", name)
		}
	}

	// Nothing may be sitting in the default partition: that would mean the
	// runway is short and inserts are landing in the catch-all.
	var inDefault int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots_default`).Scan(&inDefault); err != nil {
		t.Fatalf("read slots_default: %v", err)
	}
	if inDefault != 0 {
		t.Fatalf("%d rows landed in slots_default", inDefault)
	}
}
