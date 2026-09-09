package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
)

// NATSBroker is the JetStream-backed Publisher and Subscriber.
type NATSBroker struct {
	conn     *nats.Conn
	js       jetstream.JetStream
	stream   string
	maxBytes int64
	log      zerolog.Logger
}

var (
	_ Publisher  = (*NATSBroker)(nil)
	_ Subscriber = (*NATSBroker)(nil)
)

// NATSOptions configures the broker connection.
type NATSOptions struct {
	URL             string
	Stream          string
	CredentialsFile string
	Name            string
	MaxReconnects   int
	// MaxBytes caps the stream. Zero or negative means server-bounded, which
	// is the right default -- see ensureStream.
	MaxBytes int64
}

// NewNATS connects to NATS, enables JetStream, and ensures the platform stream
// exists with file storage so events survive a broker restart.
func NewNATS(ctx context.Context, o NATSOptions, log zerolog.Logger) (*NATSBroker, error) {
	if o.Stream == "" {
		o.Stream = "TELEMED"
	}
	maxReconnects := o.MaxReconnects
	if maxReconnects == 0 {
		maxReconnects = -1 // reconnect forever; a broker outage must not need a redeploy
	}

	opts := []nats.Option{
		nats.Name(o.Name),
		nats.MaxReconnects(maxReconnects),
		nats.ReconnectWait(2 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn().Err(err).Msg("nats disconnected")
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Info().Str("url", c.ConnectedUrl()).Msg("nats reconnected")
		}),
	}
	if o.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(o.CredentialsFile))
	}

	conn, err := nats.Connect(o.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("events: connect nats: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("events: jetstream: %w", err)
	}

	maxBytes := o.MaxBytes
	if maxBytes <= 0 {
		maxBytes = -1 // server-bounded
	}
	b := &NATSBroker{conn: conn, js: js, stream: o.Stream, maxBytes: maxBytes, log: log}
	if err := b.ensureStream(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return b, nil
}

// ensureStream creates or updates the platform stream. Every service calls it
// at boot, which makes the topology self-healing: a fresh cluster needs no
// manual provisioning step that someone will forget in 2031.
func (b *NATSBroker) ensureStream(ctx context.Context) error {
	subjects := make([]string, 0, len(AllSubjects))
	seen := map[string]struct{}{}
	for _, s := range AllSubjects {
		prefix := domainOf(string(s)) + ".>"
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		subjects = append(subjects, prefix)
	}

	cfg := jetstream.StreamConfig{
		Name:        b.stream,
		Description: "Telemedicine platform domain events",
		Subjects:    subjects,
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      7 * 24 * time.Hour,
		MaxMsgs:     -1,
		// MaxBytes is -1 (server-bounded) by default, NOT a hardcoded ceiling.
		// JetStream refuses stream creation outright when MaxBytes exceeds the
		// server's own max_storage -- and a container sizes that from the
		// available disk, so any absolute figure we pick here is wrong on some
		// machine. A 10 GiB constant took down every event-publishing service
		// at boot on a host whose store was 9.4 GiB. Retention is bounded by
		// MaxAge instead; set NATS_MAX_BYTES in production if one stream must
		// be capped below the server limit.
		MaxBytes:   b.maxBytes,
		Discard:    jetstream.DiscardOld,
		Duplicates: 2 * time.Minute, // JetStream-level dedupe on Nats-Msg-Id
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// The subject list is UNIONED with whatever the stream already carries,
	// never replaced.
	//
	// Every service calls this at boot with its own copy of AllSubjects, and
	// CreateOrUpdateStream is last-writer-wins. During a rolling deploy -- or
	// any time one service lags a release -- an older binary would otherwise
	// rewrite the stream WITHOUT the newest subjects. Publishes to those
	// subjects then fail, the outbox relay retries them forever, and the only
	// symptom is a growing backlog: no error at the publisher, no data loss,
	// nothing that points at the cause. Unioning makes the operation
	// idempotent and order-independent, which is the only safe shape when N
	// services all assert the same topology.
	if existing, err := b.js.Stream(ctx, b.stream); err == nil {
		info, ierr := existing.Info(ctx)
		if ierr == nil && info != nil {
			merged := append([]string(nil), cfg.Subjects...)
			for _, was := range info.Config.Subjects {
				if !slices.Contains(merged, was) {
					merged = append(merged, was)
				}
			}
			slices.Sort(merged)
			if len(merged) != len(cfg.Subjects) {
				b.log.Info().
					Strs("kept_from_existing_stream", info.Config.Subjects).
					Msg("preserving subjects this binary does not know about")
			}
			cfg.Subjects = merged
		}
	}

	if _, err := b.js.CreateOrUpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("events: ensure stream %s: %w", b.stream, err)
	}
	b.log.Info().Str("stream", b.stream).Strs("subjects", cfg.Subjects).Msg("jetstream stream ready")
	return nil
}

func domainOf(subject string) string {
	for i := range len(subject) {
		if subject[i] == '.' {
			return subject[:i]
		}
	}
	return subject
}

// Publish sends the envelope, using the event ID as the JetStream dedupe key so
// a relay that crashes between publish and mark-published cannot produce a
// duplicate within the dedupe window.
func (b *NATSBroker) Publish(ctx context.Context, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}

	msg := &nats.Msg{
		Subject: string(env.Subject),
		Data:    body,
		Header:  nats.Header{},
	}
	msg.Header.Set(jetstream.MsgIDHeader, env.ID.String())
	msg.Header.Set("Telemed-Producer", env.Producer)
	msg.Header.Set("Telemed-Version", fmt.Sprintf("%d", env.Version))

	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("events: publish %s: %w", env.Subject, err)
	}
	return nil
}

// Subscribe creates a durable pull consumer and dispatches to h. It blocks
// until ctx is cancelled.
func (b *NATSBroker) Subscribe(ctx context.Context, durable string, subjects []Subject, h Handler) error {
	if err := validateDurable(durable); err != nil {
		return err
	}

	filters := make([]string, len(subjects))
	for i, s := range subjects {
		filters[i] = string(s)
	}

	cons, err := b.js.CreateOrUpdateConsumer(ctx, b.stream, jetstream.ConsumerConfig{
		Durable:        durable,
		Description:    "durable consumer for " + durable,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        30 * time.Second,
		MaxDeliver:     5, // then it lands in the dead-letter path via advisory
		FilterSubjects: filters,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		BackOff:        []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second},
	})
	if err != nil {
		return fmt.Errorf("events: create consumer %s: %w", durable, err)
	}

	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			// A malformed envelope will never become valid on redelivery.
			b.log.Error().Err(err).Str("subject", msg.Subject()).Msg("dropping unparseable event")
			_ = msg.Term()
			return
		}

		handlerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		defer cancel()

		if err := h(handlerCtx, env); err != nil {
			b.log.Error().Err(err).
				Str("subject", string(env.Subject)).
				Str("event_id", env.ID.String()).
				Msg("event handler failed, will redeliver")
			_ = msg.Nak()
			return
		}
		if err := msg.Ack(); err != nil {
			b.log.Warn().Err(err).Str("event_id", env.ID.String()).Msg("ack failed")
		}
	})
	if err != nil {
		return fmt.Errorf("events: consume %s: %w", durable, err)
	}
	defer consumeCtx.Stop()

	b.log.Info().Str("durable", durable).Strs("subjects", filters).Msg("event subscriber running")
	<-ctx.Done()
	return nil
}

// Close drains in-flight messages and closes the connection.
func (b *NATSBroker) Close() error {
	if err := b.conn.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return fmt.Errorf("events: drain nats: %w", err)
	}
	return nil
}

// validateDurable rejects a durable name JetStream will not accept.
//
// This is checked here, loudly, because the failure mode is otherwise invisible
// in effect: a durable containing "." is refused by the server, the consumer
// never starts, and the only symptom is a projection that silently stays empty.
// On this platform that presented as doctor search returning no availability
// against 1,577 real slots -- with nothing in the logs pointing at NATS.
func validateDurable(name string) error {
	if name == "" {
		return fmt.Errorf("events: durable name must not be empty")
	}
	for _, bad := range []string{".", "*", ">", " ", "\t", "\n", "/", "\\"} {
		if strings.Contains(name, bad) {
			return fmt.Errorf("events: durable name %q contains %q, which JetStream "+
				"rejects; use '_' or '-' instead", name, bad)
		}
	}
	return nil
}
