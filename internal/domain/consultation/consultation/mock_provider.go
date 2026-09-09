package consultation

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MockProvider is a fully in-process VideoProvider. It exists so the service
// is testable and runnable end to end with no LiveKit server, no network
// call, and no flaky external dependency -- exactly what a laptop with 11GB
// free and a contended Docker daemon needs.
//
// It is not a stub: rooms, tokens, and egress jobs are real in-memory state
// with the same invariants LiveKit enforces (idempotent room creation, a
// room-scoped and time-limited token, participants that only appear after a
// token was issued for them). Webhook verification is a simple shared-secret
// check rather than LiveKit's JWT+checksum scheme, since MockProvider never
// receives a real signed webhook -- tests that need to exercise verification
// failure drive it directly via WithWebhookSecret and a wrong secret.
type MockProvider struct {
	mu           sync.Mutex
	webhookToken string
	rooms        map[string]*mockRoom
	egresses     map[string]*mockEgress
	events       []WebhookEvent // recorded for assertions in tests
}

type mockRoom struct {
	name         string
	createdAt    time.Time
	participants map[string]mockParticipant
	deleted      bool
}

type mockParticipant struct {
	identity  string
	joinedAt  time.Time
	publisher bool
}

type mockEgress struct {
	id       string
	roomName string
	bucket   string
	key      string
	status   string
	stopped  bool
}

var _ VideoProvider = (*MockProvider)(nil)

// NewMockProvider builds a MockProvider. webhookToken is the bearer value
// VerifyWebhook requires in the Authorization header -- analogous to the
// LiveKit-signed JWT the real provider checks, simplified to something a test
// can construct without a key pair.
func NewMockProvider(webhookToken string) *MockProvider {
	if webhookToken == "" {
		// Not a credential: the mock provider is the VIDEO_PROVIDER=mock
		// implementation used by tests and local dev. This literal is the
		// fallback bearer value a test compares against when no secret was
		// supplied; it authenticates nothing outside this process.
		webhookToken = "mock-webhook-token" //nolint:gosec // G101: fixture value for the in-memory mock provider, not a real secret
	}
	return &MockProvider{
		webhookToken: webhookToken,
		rooms:        map[string]*mockRoom{},
		egresses:     map[string]*mockEgress{},
	}
}

func (m *MockProvider) Name() string { return "mock" }

func (m *MockProvider) CreateRoom(_ context.Context, spec RoomSpec) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if r, ok := m.rooms[spec.RoomName]; ok && !r.deleted {
		return Room{Name: r.name, SID: "RM_" + r.name, CreatedAt: r.createdAt}, nil
	}
	r := &mockRoom{
		name:         spec.RoomName,
		createdAt:    time.Now().UTC(),
		participants: map[string]mockParticipant{},
	}
	m.rooms[spec.RoomName] = r
	return Room{Name: r.name, SID: "RM_" + r.name, CreatedAt: r.createdAt}, nil
}

func (m *MockProvider) GenerateToken(_ context.Context, spec TokenSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[spec.RoomName]
	if !ok || r.deleted {
		return "", fmt.Errorf("consultation: mock generate token: %w", ErrRoomNotFound)
	}
	r.participants[spec.Identity] = mockParticipant{
		identity:  spec.Identity,
		joinedAt:  time.Now().UTC(),
		publisher: spec.CanPublish,
	}

	ttl := spec.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	// The mock token is a deterministic, inspectable stand-in for a real JWT:
	// it is never verified by anything (LiveKit itself would verify a real
	// one), it only needs to be unique and to carry enough for a test or a
	// developer at a REPL to eyeball what it grants.
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("mock.%s.%s.%s.%s.%d",
		spec.RoomName, spec.Identity, hex.EncodeToString(buf),
		publishFlag(spec.CanPublish), time.Now().Add(ttl).Unix()), nil
}

func publishFlag(canPublish bool) string {
	if canPublish {
		return "pub"
	}
	return "sub"
}

func (m *MockProvider) EndRoom(_ context.Context, roomName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[roomName]
	if !ok || r.deleted {
		return fmt.Errorf("consultation: mock end room %s: %w", roomName, ErrRoomNotFound)
	}
	r.deleted = true
	return nil
}

func (m *MockProvider) ListParticipants(_ context.Context, roomName string) ([]ProviderParticipant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[roomName]
	if !ok || r.deleted {
		return nil, fmt.Errorf("consultation: mock list participants %s: %w", roomName, ErrRoomNotFound)
	}
	out := make([]ProviderParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, ProviderParticipant{
			Identity:        p.identity,
			JoinedAt:        p.joinedAt,
			IsPublisher:     p.publisher,
			ConnectionState: "ACTIVE",
		})
	}
	return out, nil
}

func (m *MockProvider) RemoveParticipant(_ context.Context, roomName, identity string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[roomName]
	if !ok || r.deleted {
		return fmt.Errorf("consultation: mock remove participant %s: %w", roomName, ErrRoomNotFound)
	}
	delete(r.participants, identity)
	return nil
}

func (m *MockProvider) StartRecording(_ context.Context, spec RecordingSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.rooms[spec.RoomName]; !ok {
		return "", fmt.Errorf("consultation: mock start recording %s: %w", spec.RoomName, ErrRoomNotFound)
	}
	id := "EG_" + hex.EncodeToString(randBytes(6))
	m.egresses[id] = &mockEgress{
		id:       id,
		roomName: spec.RoomName,
		bucket:   spec.Bucket,
		key:      spec.OutputKey,
		status:   EgressStatusActive,
	}
	return id, nil
}

func (m *MockProvider) StopRecording(_ context.Context, egressID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.egresses[egressID]
	if !ok {
		return fmt.Errorf("consultation: mock stop recording %s: egress not found", egressID)
	}
	e.stopped = true
	e.status = EgressStatusComplete
	return nil
}

// VerifyWebhook checks the simple bearer scheme described on MockProvider and
// decodes the JSON body directly (the mock's "webhook sender", used by tests
// and by scripts/simulate-webhook, posts plain JSON rather than LiveKit's
// protobuf-JSON WebhookEvent).
func (m *MockProvider) VerifyWebhook(_ context.Context, authHeader string, body []byte) (WebhookEvent, error) {
	// Constant-time, even though this is the mock: the comparison is on the
	// only thing standing between an anonymous POST and a call being force-
	// ended, and "it is only the mock" is exactly the assumption that made an
	// unset VIDEO_PROVIDER dangerous in the first place.
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if authHeader == "" || subtle.ConstantTimeCompare([]byte(token), []byte(m.webhookToken)) != 1 {
		return WebhookEvent{}, fmt.Errorf("consultation: mock verify webhook: %w", ErrWebhookUnverified)
	}
	var evt WebhookEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return WebhookEvent{}, fmt.Errorf("consultation: mock decode webhook body: %w", err)
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = time.Now().UTC()
	}

	m.mu.Lock()
	m.events = append(m.events, evt)
	m.mu.Unlock()
	return evt, nil
}

// WebhookToken exposes the bearer token so tests and the local run script can
// construct a valid Authorization header.
func (m *MockProvider) WebhookToken() string { return m.webhookToken }

// RoomExists reports whether CreateRoom has been called for name and EndRoom
// has not since. Used by tests to assert on provider-side side effects.
func (m *MockProvider) RoomExists(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[name]
	return ok && !r.deleted
}

// EgressState is the observable state of one recording job.
type EgressState struct {
	RoomName string
	Bucket   string
	Key      string
	Status   string
	Stopped  bool
}

// Egress exposes an in-flight or finished recording job for assertions.
// ok is false when no such egress was ever started.
func (m *MockProvider) Egress(id string) (state EgressState, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, found := m.egresses[id]
	if !found {
		return EgressState{}, false
	}
	return EgressState{
		RoomName: e.roomName,
		Bucket:   e.bucket,
		Key:      e.key,
		Status:   e.status,
		Stopped:  e.stopped,
	}, true
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
