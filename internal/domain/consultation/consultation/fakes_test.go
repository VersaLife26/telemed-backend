package consultation

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
)

// --- fake transaction / pool -------------------------------------------
//
// database.InTx needs a real pgx.Tx-shaped value to open, commit, and roll
// back. fakeStore below never actually calls a method on the tx/queryer
// argument it is handed -- it reads and writes an in-memory map instead -- so
// fakeTx only needs to satisfy the interface and answer Commit/Rollback
// correctly for InTx's own lifecycle bookkeeping.

type fakeTx struct{}

func (fakeTx) Begin(context.Context) (pgx.Tx, error) { return fakeTx{}, nil }
func (fakeTx) Commit(context.Context) error          { return nil }
func (fakeTx) Rollback(context.Context) error        { return nil }
func (fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (fakeTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakeTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }
func (fakeTx) Conn() *pgx.Conn                                         { return nil }

type fakePool struct{}

func (fakePool) Begin(context.Context) (pgx.Tx, error)                  { return fakeTx{}, nil }
func (fakePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return fakeTx{}, nil }
func (fakePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakePool) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakePool) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }
func (fakePool) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (fakePool) Ping(context.Context) error { return nil }
func (fakePool) Close()                     {}

var _ database.Pool = fakePool{}

// --- fake store ----------------------------------------------------------
//
// fakeStore is an in-memory implementation of the store interface Service
// depends on. It exists so every service-layer rule -- token authorization,
// consent gating, waiting-room ordering, webhook idempotency, the state
// machine including the abandoned path -- can be exercised with go test
// alone, no Postgres required.

type fakeStore struct {
	mu sync.Mutex

	consultations map[uuid.UUID]*Consultation
	byAppointment map[uuid.UUID]uuid.UUID
	byRoom        map[string]uuid.UUID

	participants map[uuid.UUID]map[string]*Participant
	consents     []Consent
	events       []Event

	waitingRoom map[uuid.UUID]*WaitingRoomEntry

	webhookReceipts map[string]bool

	// completedDurations mimics "SELECT duration_seconds FROM consultations
	// WHERE doctor_id=... AND status='ended' ORDER BY ended_at DESC" -- one
	// slice per doctor, appended in the order consultations actually ended.
	completedDurations map[uuid.UUID][]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		consultations:      map[uuid.UUID]*Consultation{},
		byAppointment:      map[uuid.UUID]uuid.UUID{},
		byRoom:             map[string]uuid.UUID{},
		participants:       map[uuid.UUID]map[string]*Participant{},
		waitingRoom:        map[uuid.UUID]*WaitingRoomEntry{},
		webhookReceipts:    map[string]bool{},
		completedDurations: map[uuid.UUID][]int{},
	}
}

var _ store = (*fakeStore)(nil)

func ptrTime(t time.Time) *time.Time { return &t }

func (f *fakeStore) CreateConsultation(_ context.Context, _ pgx.Tx, c *Consultation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.byAppointment[c.AppointmentID]; exists {
		return ErrDuplicateConsultation
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	c.Version = 0
	if c.RecordingStatus == "" {
		c.RecordingStatus = RecordingNone
	}
	if c.ScheduledEndAt.IsZero() || !c.ScheduledEndAt.After(c.ScheduledAt) {
		c.ScheduledEndAt = c.ScheduledAt.Add(15 * time.Minute)
	}

	cp := *c
	f.consultations[c.ID] = &cp
	f.byAppointment[c.AppointmentID] = c.ID
	f.byRoom[c.RoomName] = c.ID
	return nil
}

func (f *fakeStore) GetConsultation(_ context.Context, _ queryer, id uuid.UUID) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consultations[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeStore) GetConsultationByAppointment(_ context.Context, _ queryer, appointmentID uuid.UUID) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byAppointment[appointmentID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *f.consultations[id]
	return &cp, nil
}

func (f *fakeStore) GetConsultationByRoomName(_ context.Context, _ queryer, roomName string) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byRoom[roomName]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *f.consultations[id]
	return &cp, nil
}

func (f *fakeStore) UpdateConsultation(_ context.Context, _ pgx.Tx, c *Consultation) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.consultations[c.ID]
	if !ok {
		return ErrNotFound
	}
	if existing.Version != c.Version {
		return ErrOptimisticLock
	}

	updated := *c
	updated.Version = c.Version + 1
	updated.UpdatedAt = time.Now().UTC()
	// Fields the caller never mutates directly stay pinned to what CreateConsultation set.
	updated.CreatedAt = existing.CreatedAt
	updated.AppointmentID = existing.AppointmentID
	updated.PatientID = existing.PatientID
	updated.DoctorID = existing.DoctorID
	updated.RoomName = existing.RoomName

	f.consultations[c.ID] = &updated
	c.Version = updated.Version
	c.UpdatedAt = updated.UpdatedAt

	if updated.Status == StatusEnded && updated.DurationSeconds != nil {
		f.completedDurations[updated.DoctorID] = append(f.completedDurations[updated.DoctorID], *updated.DurationSeconds)
	}
	return nil
}

func (f *fakeStore) UpdateConsultationScheduledAt(_ context.Context, _ pgx.Tx, appointmentID uuid.UUID, startAt, endAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.byAppointment[appointmentID]
	if !ok {
		return nil
	}
	existing := f.consultations[id]
	if existing.Status != StatusScheduled && existing.Status != StatusWaiting {
		return nil
	}
	if !endAt.After(startAt) {
		endAt = startAt.Add(15 * time.Minute)
	}
	updated := *existing
	updated.ScheduledAt = startAt.UTC()
	updated.ScheduledEndAt = endAt.UTC()
	updated.RunningLateNotifiedAt = nil
	updated.EarlyJoinOfferedAt = nil
	updated.EarlyJoinResponse = nil
	updated.EarlyJoinRespondedAt = nil
	updated.Version = existing.Version + 1
	updated.UpdatedAt = time.Now().UTC()
	f.consultations[id] = &updated
	return nil
}

func (f *fakeStore) ListOverrunActive(_ context.Context, _ queryer, now time.Time, limit int) ([]*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	var out []*Consultation
	for _, c := range f.consultations {
		if c.Status != StatusActive || c.DeletedAt != nil {
			continue
		}
		if c.RunningLateNotifiedAt != nil {
			continue
		}
		if !c.ScheduledEndAt.Before(now) {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ScheduledEndAt.Before(out[j].ScheduledEndAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) FindNextUpcomingForDoctor(_ context.Context, _ queryer, doctorID uuid.UUID, afterScheduledAt time.Time) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *Consultation
	for _, c := range f.consultations {
		if c.DoctorID != doctorID || c.DeletedAt != nil {
			continue
		}
		if c.Status != StatusScheduled && c.Status != StatusWaiting {
			continue
		}
		if !c.ScheduledAt.After(afterScheduledAt) {
			continue
		}
		if best == nil || c.ScheduledAt.Before(best.ScheduledAt) {
			cp := *c
			best = &cp
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func (f *fakeStore) ClaimRunningLateNotified(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consultations[consultationID]
	if !ok || c.Status != StatusActive || c.DeletedAt != nil || c.RunningLateNotifiedAt != nil {
		return false, nil
	}
	ts := at.UTC()
	c.RunningLateNotifiedAt = &ts
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	return true, nil
}

func (f *fakeStore) FindActiveForDoctor(_ context.Context, _ queryer, doctorID uuid.UUID) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.consultations {
		if c.DoctorID != doctorID || c.DeletedAt != nil || c.Status != StatusActive {
			continue
		}
		cp := *c
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (f *fakeStore) FindLatestTerminalForDoctor(_ context.Context, _ queryer, doctorID uuid.UUID) (*Consultation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *Consultation
	var bestAt time.Time
	for _, c := range f.consultations {
		if c.DoctorID != doctorID || c.DeletedAt != nil {
			continue
		}
		if c.Status != StatusEnded && c.Status != StatusAbandoned && c.Status != StatusFailed {
			continue
		}
		at := c.UpdatedAt
		if c.EndedAt != nil {
			at = *c.EndedAt
		}
		if best == nil || at.After(bestAt) {
			cp := *c
			best = &cp
			bestAt = at
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func (f *fakeStore) ClaimEarlyJoinOffered(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consultations[consultationID]
	if !ok || c.DeletedAt != nil || c.EarlyJoinOfferedAt != nil {
		return false, nil
	}
	if c.Status != StatusScheduled && c.Status != StatusWaiting {
		return false, nil
	}
	ts := at.UTC()
	c.EarlyJoinOfferedAt = &ts
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	return true, nil
}

func (f *fakeStore) SetEarlyJoinResponse(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, response string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consultations[consultationID]
	if !ok || c.DeletedAt != nil || c.EarlyJoinOfferedAt == nil || c.EarlyJoinResponse != nil {
		return false, nil
	}
	resp := response
	ts := at.UTC()
	c.EarlyJoinResponse = &resp
	c.EarlyJoinRespondedAt = &ts
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	return true, nil
}

func (f *fakeStore) UpsertParticipantJoin(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, identity string, role ParticipantRole, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	m, ok := f.participants[consultationID]
	if !ok {
		m = map[string]*Participant{}
		f.participants[consultationID] = m
	}
	p, exists := m[identity]
	if !exists {
		m[identity] = &Participant{ID: uuid.New(), ConsultationID: consultationID, Identity: identity, Role: role, JoinedAt: ptrTime(at)}
		return nil
	}
	if p.JoinedAt != nil {
		p.ReconnectCount++
	}
	p.JoinedAt = ptrTime(at)
	p.LeftAt = nil
	return nil
}

func (f *fakeStore) MarkParticipantLeft(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, identity string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.participants[consultationID]
	if !ok {
		return nil
	}
	p, ok := m[identity]
	if !ok {
		return nil
	}
	p.LeftAt = ptrTime(at)
	return nil
}

func (f *fakeStore) ListParticipants(_ context.Context, _ queryer, consultationID uuid.UUID) ([]Participant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.participants[consultationID]
	out := make([]Participant, 0, len(m))
	for _, p := range m {
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeStore) SaveConsent(_ context.Context, _ pgx.Tx, c Consent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	f.consents = append(f.consents, c)
	return nil
}

// RecordingConsentStatus mirrors "DISTINCT ON (user_id) ... ORDER BY
// granted_at DESC": consents are appended in call order and GrantedAt only
// increases within a test, so a later entry in the slice always overwrites
// an earlier one for the same user -- last write wins, same as the real
// query's "most recent decision".
func (f *fakeStore) RecordingConsentStatus(_ context.Context, _ queryer, consultationID uuid.UUID) (map[uuid.UUID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for i := range f.consents {
		c := &f.consents[i]
		if c.ConsultationID != consultationID || c.Type != ConsentRecording {
			continue
		}
		out[c.UserID] = c.Granted
	}
	return out, nil
}

func (f *fakeStore) RecordEvent(_ context.Context, _ pgx.Tx, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = int64(len(f.events) + 1)
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) RecentQualityEvents(_ context.Context, _ queryer, consultationID uuid.UUID, identity string, limit int) ([]Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []Event
	for i := len(f.events) - 1; i >= 0 && len(matched) < limit; i-- {
		e := f.events[i]
		if e.ConsultationID == consultationID && e.ActorIdentity == identity && e.Type == EventQualitySample {
			matched = append(matched, e)
		}
	}
	return matched, nil
}

func (f *fakeStore) UpsertWaitingRoomEntry(_ context.Context, _ pgx.Tx, e WaitingRoomEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	e.Status = WaitingStatusWaiting
	e.AdmittedAt = nil
	e.LeftAt = nil
	f.waitingRoom[e.ConsultationID] = &e
	return nil
}

func (f *fakeStore) GetWaitingRoomEntry(_ context.Context, _ queryer, consultationID uuid.UUID) (*WaitingRoomEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.waitingRoom[consultationID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (f *fakeStore) MarkWaitingRoomAdmitted(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.waitingRoom[consultationID]
	if !ok {
		return nil
	}
	e.Status = WaitingStatusAdmitted
	e.AdmittedAt = ptrTime(at)
	return nil
}

func (f *fakeStore) MarkWaitingRoomLeft(_ context.Context, _ pgx.Tx, consultationID uuid.UUID, at time.Time, status WaitingRoomStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.waitingRoom[consultationID]
	if !ok || e.Status != WaitingStatusWaiting {
		return nil
	}
	e.Status = status
	e.LeftAt = ptrTime(at)
	return nil
}

func (f *fakeStore) CountWaitingAhead(_ context.Context, _ queryer, doctorID uuid.UUID, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, e := range f.waitingRoom {
		if e.DoctorID == doctorID && e.Status == WaitingStatusWaiting && e.EnteredAt.Before(before) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) AverageDurationSeconds(_ context.Context, _ queryer, doctorID uuid.UUID, sampleSize int) (avgSeconds float64, ok bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	durations := f.completedDurations[doctorID]
	if len(durations) == 0 {
		return 0, false, nil
	}
	start := 0
	if len(durations) > sampleSize {
		start = len(durations) - sampleSize
	}
	recent := durations[start:]
	sum := 0
	for _, d := range recent {
		sum += d
	}
	return float64(sum) / float64(len(recent)), true, nil
}

func (f *fakeStore) ClaimWebhookEvent(_ context.Context, _ pgx.Tx, eventID, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.webhookReceipts[eventID] {
		return false, nil
	}
	f.webhookReceipts[eventID] = true
	return true, nil
}

// --- fake cache ------------------------------------------------------------
//
// fakeCache is a minimal in-memory Cache good enough to back the waiting
// room's sorted-set operations in tests. Setting alwaysMissZRank simulates a
// Redis flush so tests can exercise the Postgres fallback path (ADR-007)
// deterministically.

type fakeCache struct {
	mu              sync.Mutex
	sets            map[string]map[string]float64
	alwaysMissZRank bool
}

func newFakeCache() *fakeCache { return &fakeCache{sets: map[string]map[string]float64{}} }

var _ cache.Cache = (*fakeCache)(nil)

func (c *fakeCache) Get(context.Context, string) ([]byte, error)              { return nil, cache.ErrNotFound }
func (c *fakeCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (c *fakeCache) Del(context.Context, ...string) error                     { return nil }
func (c *fakeCache) Exists(context.Context, string) (bool, error)             { return false, nil }
func (c *fakeCache) Incr(context.Context, string, time.Duration) (int64, error) {
	return 1, nil
}
func (c *fakeCache) Lock(context.Context, string, time.Duration) (token string, acquired bool, err error) {
	return "token", true, nil
}
func (c *fakeCache) Unlock(context.Context, string, string) error { return nil }

func (c *fakeCache) ZAdd(_ context.Context, key string, score float64, member string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.sets[key]
	if !ok {
		m = map[string]float64{}
		c.sets[key] = m
	}
	m[member] = score
	return nil
}

func (c *fakeCache) ZRem(_ context.Context, key string, members ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.sets[key]
	if !ok {
		return nil
	}
	for _, mem := range members {
		delete(m, mem)
	}
	return nil
}

// Expire records the TTL. The fakes do not evict on it -- no test here
// depends on expiry happening, only on the call being made.
func (c *fakeCache) Expire(_ context.Context, _ string, _ time.Duration) error { return nil }

func (c *fakeCache) ZRank(_ context.Context, key, member string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alwaysMissZRank {
		return 0, cache.ErrNotFound
	}
	m, ok := c.sets[key]
	if !ok {
		return 0, cache.ErrNotFound
	}
	score, ok := m[member]
	if !ok {
		return 0, cache.ErrNotFound
	}
	var rank int64
	for mem, sc := range m {
		if mem == member {
			continue
		}
		if sc < score {
			rank++
		}
	}
	return rank, nil
}

func (c *fakeCache) ZRange(context.Context, string, int64, int64) ([]string, error) {
	return nil, nil // unused by service.go
}

func (c *fakeCache) ZCard(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.sets[key])), nil
}

func (c *fakeCache) Ping(context.Context) error { return nil }
func (c *fakeCache) Close() error               { return nil }

// timeline returns a copy of every event recorded so far. Tests assert on it
// where the fake transaction makes the outbox unobservable: the timeline row
// and the outbox row are written inside the same transaction, so the presence
// of one is a faithful proxy for the presence of the other.
func (f *fakeStore) timeline() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}

// ListStaleActive mirrors the SQL in repository.go: active consultations whose
// latest evidence of activity -- started_at, the last participant join/leave,
// or the last timeline event -- is older than quietSince.
func (f *fakeStore) ListStaleActive(_ context.Context, _ queryer, quietSince time.Time, limit int) ([]StaleCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := []StaleCandidate{}
	for _, c := range f.consultations {
		if c.Status != StatusActive || c.DeletedAt != nil || c.StartedAt == nil {
			continue
		}
		if !c.StartedAt.Before(quietSince) {
			continue
		}
		last := *c.StartedAt
		for _, p := range f.participants[c.ID] {
			if p.JoinedAt != nil && p.JoinedAt.After(last) {
				last = *p.JoinedAt
			}
			if p.LeftAt != nil && p.LeftAt.After(last) {
				last = *p.LeftAt
			}
		}
		for i := range f.events {
			if f.events[i].ConsultationID == c.ID && f.events[i].OccurredAt.After(last) {
				last = f.events[i].OccurredAt
			}
		}
		if last.After(quietSince) {
			continue
		}
		out = append(out, StaleCandidate{
			ID: c.ID, RoomName: c.RoomName, StartedAt: *c.StartedAt, LastActivity: last,
		})
		if len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}
