package notification

import "context"

// Registry is the default ProviderRegistry: one NotificationProvider
// instance per channel, selected at boot time from config (ADR-002:
// selection is config-driven and per-channel). It applies no fallback logic
// of its own -- a channel with nothing registered fails dispatch honestly
// (permanent error, dead-lettered) rather than silently dropping messages.
type Registry struct {
	byChannel map[Channel]NotificationProvider
}

// NewRegistry returns an empty registry ready for Register calls.
func NewRegistry() *Registry {
	return &Registry{byChannel: make(map[Channel]NotificationProvider)}
}

// Register binds provider to every channel it declares support for via
// Channels(). A later Register call for the same channel replaces the
// earlier one, so callers should register in the order config resolves
// (e.g. console defaults first, then any explicitly configured overrides).
func (r *Registry) Register(p NotificationProvider) {
	for _, ch := range p.Channels() {
		r.byChannel[ch] = p
	}
}

// ProviderFor implements ProviderRegistry.
func (r *Registry) ProviderFor(ch Channel) (NotificationProvider, bool) {
	p, ok := r.byChannel[ch]
	return p, ok
}

var _ ProviderRegistry = (*Registry)(nil)

// InAppProvider "delivers" in_app notifications by doing nothing: the
// notification row itself, readable via GET /api/v1/notifications, is the
// delivery. It still goes through the same NotificationProvider interface
// and the same dispatch/retry pipeline as every other channel so the rest
// of the service never special-cases in_app.
type InAppProvider struct{}

// NewInApp returns the in_app channel's (trivial, always-available)
// provider.
func NewInApp() InAppProvider { return InAppProvider{} }

func (InAppProvider) Send(_ context.Context, _ Message) (Receipt, error) {
	return Receipt{Delivered: true}, nil
}

func (InAppProvider) Channels() []Channel { return []Channel{ChannelInApp} }
func (InAppProvider) Name() string        { return "in-app" }

var _ NotificationProvider = InAppProvider{}
