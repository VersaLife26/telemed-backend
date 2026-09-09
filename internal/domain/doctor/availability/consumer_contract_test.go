package availability

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// TestSlotEventPayloadMatchesCanonical is the guard that makes this package's
// narrow SlotEventPayload safe.
//
// The projection decodes a subset of the canonical payloads rather than the
// whole struct, because it only needs four fields. The risk in doing that is
// exactly the risk this whole reconciliation pass exists to remove: if
// scheduling-service renames start_at, encoding/json will not complain, the
// field will decode to the zero time, and every slot will be bucketed into
// year 1. So the subset is pinned here against the real producer types.
func TestSlotEventPayloadMatchesCanonical(t *testing.T) {
	slotID, doctorID, apptID := uuid.New(), uuid.New(), uuid.New()
	start := time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	t.Run("slot.booked", func(t *testing.T) {
		raw, err := json.Marshal(events.SlotBooked{
			SlotID: slotID, DoctorID: doctorID, AppointmentID: apptID,
			StartAt: start, EndAt: end,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertDecodes(t, raw, slotID, doctorID, start, end)
	})

	t.Run("slot.released", func(t *testing.T) {
		raw, err := json.Marshal(events.SlotReleased{
			SlotID: slotID, DoctorID: doctorID,
			StartAt: start, EndAt: end, Reason: "cancelled",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertDecodes(t, raw, slotID, doctorID, start, end)
	})

	t.Run("slot.withdrawn", func(t *testing.T) {
		raw, err := json.Marshal(events.SlotWithdrawn{
			SlotID: slotID, DoctorID: doctorID,
			StartAt: start, EndAt: end, Reason: "holiday",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertDecodes(t, raw, slotID, doctorID, start, end)
	})
}

// TestWithdrawnIsNotFoldedIntoReleased keeps slot.withdrawn mapping to a status
// that is not AVAILABLE.
//
// The two subjects are opposites and the mapping is one line, which is exactly
// the kind of line a future edit collapses "for simplicity". Doing so would put
// a doctor's slots back on the market on the day they are on leave.
func TestWithdrawnIsNotFoldedIntoReleased(t *testing.T) {
	withdrawn, ok := statusFor(events.SubjectSlotWithdrawn)
	if !ok {
		t.Fatal("slot.withdrawn maps to no status; the projection would drop it")
	}
	if withdrawn == SlotAvailable {
		t.Fatal("slot.withdrawn maps to AVAILABLE: a doctor on leave stays bookable")
	}
	if withdrawn != SlotWithdrawn {
		t.Errorf("slot.withdrawn maps to %q, want WITHDRAWN", withdrawn)
	}

	released, _ := statusFor(events.SubjectSlotReleased)
	if released != SlotAvailable {
		t.Errorf("slot.released maps to %q, want AVAILABLE", released)
	}
}

func assertDecodes(t *testing.T, raw []byte, slotID, doctorID uuid.UUID, start, end time.Time) {
	t.Helper()
	var got SlotEventPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SlotID != slotID {
		t.Errorf("slot_id = %s, want %s", got.SlotID, slotID)
	}
	if got.DoctorID != doctorID {
		t.Errorf("doctor_id = %s, want %s", got.DoctorID, doctorID)
	}
	if !got.StartAt.Equal(start) {
		t.Errorf("start_at = %s, want %s", got.StartAt, start)
	}
	if !got.EndAt.Equal(end) {
		t.Errorf("end_at = %s, want %s", got.EndAt, end)
	}
}

// TestSlotsGeneratedCarriesNoSlotID documents, executably, why this projection
// no longer subscribes to slot.generated. If a future version of the canonical
// payload gains a slot id, this test fails and tells whoever added it that the
// subscription can and should come back.
func TestSlotsGeneratedCarriesNoSlotID(t *testing.T) {
	raw, err := json.Marshal(events.SlotsGenerated{
		DoctorID: uuid.New(), FromDate: "2026-08-21", ToDate: "2026-09-20", Count: 480,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["slot_id"]; ok {
		t.Fatal("events.SlotsGenerated now carries slot_id: this projection can " +
			"subscribe to slot.generated again -- see Consumer.Subjects")
	}

	for _, subject := range (&Consumer{}).Subjects() {
		if subject == events.SubjectSlotsGenerated {
			t.Fatal("slot.generated is subscribed but carries no slot_id; every " +
				"delivery would be dropped")
		}
	}
}
