package scheduling

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the scheduling-specific instruments. Their names are an
// operational contract: the runbook and the alert rules reference them by
// string, so renaming one silently breaks a page.
type Metrics struct {
	// Bookings is labelled by outcome, which is what makes the contention
	// story legible: a rising slot_locked share means the herd is being turned
	// away cheaply, a rising slot_unavailable share means it is not.
	Bookings        *prometheus.CounterVec
	BookingDuration prometheus.Histogram

	// VersionConflicts and UniqueViolations are the two deeper defence layers
	// firing. In steady state both are zero. A non-zero UniqueViolations means
	// something reached the insert without the row lock, and is worth waking
	// somebody for.
	VersionConflicts prometheus.Counter
	UniqueViolations prometheus.Counter

	SlotsGenerated     prometheus.Counter
	SlotsArchived      prometheus.Counter
	WaitlistPromotions *prometheus.CounterVec
	// DoubleBookedSlots is set by the invariant checker. It must be 0.
	DoubleBookedSlots prometheus.Gauge
}

// NewMetrics registers the instruments. Passing nil gives a private registry,
// which is what tests want: no global state, no duplicate-registration panic on
// the second construction.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{
		Bookings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_scheduling_bookings_total",
			Help: "Booking attempts by outcome (success, slot_unavailable, slot_locked, error).",
		}, []string{"outcome"}),
		BookingDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "telemed_scheduling_booking_duration_seconds",
			Help: "End-to-end BookSlot latency, including the Redis reject and the transaction.",
			// The tail matters more than the median here: a booking that takes
			// two seconds means the row lock is queueing.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
		VersionConflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemed_scheduling_version_conflicts_total",
			Help: "Optimistic-lock misses on slots. Non-zero means real contention reached Postgres.",
		}),
		UniqueViolations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemed_scheduling_unique_violations_total",
			Help: "Bookings stopped by uq_appointments_slot_live. Should be 0; page if sustained.",
		}),
		SlotsGenerated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemed_scheduling_slots_generated_total",
			Help: "Slots materialised by the generation job.",
		}),
		SlotsArchived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemed_scheduling_slots_archived_total",
			Help: "Slots moved to slots_archive.",
		}),
		WaitlistPromotions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_scheduling_waitlist_promotions_total",
			Help: "Waitlist offers by outcome (offered, no_candidates, expired).",
		}, []string{"outcome"}),
		DoubleBookedSlots: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemed_scheduling_double_booked_slots",
			Help: "Slots holding more than one live appointment. Must be 0. Page immediately.",
		}),
	}
	reg.MustRegister(m.Bookings, m.BookingDuration, m.VersionConflicts, m.UniqueViolations,
		m.SlotsGenerated, m.SlotsArchived, m.WaitlistPromotions, m.DoubleBookedSlots)
	return m
}

// observeBooking records one booking attempt.
func (m *Metrics) observeBooking(err error, d time.Duration) {
	m.BookingDuration.Observe(d.Seconds())
	switch {
	case err == nil:
		m.Bookings.WithLabelValues("success").Inc()
	case errors.Is(err, ErrSlotLocked):
		m.Bookings.WithLabelValues("slot_locked").Inc()
	case errors.Is(err, ErrSlotUnavailable), errors.Is(err, ErrVersionConflict), errors.Is(err, ErrSlotReserved):
		m.Bookings.WithLabelValues("slot_unavailable").Inc()
	case errors.Is(err, ErrDuplicateBooking):
		m.Bookings.WithLabelValues("duplicate").Inc()
	case errors.Is(err, ErrSlotNotFound), errors.Is(err, ErrSlotInPast):
		m.Bookings.WithLabelValues("rejected").Inc()
	default:
		m.Bookings.WithLabelValues("error").Inc()
	}
}
