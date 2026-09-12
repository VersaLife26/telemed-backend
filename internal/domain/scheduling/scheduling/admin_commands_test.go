package scheduling_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/events"
)

// The two tests below exist because the failure they catch is silent.
//
// admin-service enqueues these commands, the outbox relays them, JetStream
// retains them, and this service is the only thing that can carry them out.
// If it is not subscribed, or if the payload no longer decodes, the
// administrator still sees a success in the console and the audit log still
// records the decision -- the appointment simply never gets cancelled. There
// is no error anywhere to notice.

func TestConsumersSubscribeToAdminCommands(t *testing.T) {
	subscribed := map[events.Subject]bool{}
	for _, s := range (&scheduling.Consumers{}).Subjects() {
		subscribed[s] = true
	}

	for _, want := range []events.Subject{
		events.SubjectAdminAppointmentForceCancel,
		events.SubjectAdminDoubleBookingResolveRequested,
		events.SubjectConsultationPatientNoShow,
	} {
		if !subscribed[want] {
			t.Errorf("scheduling does not subscribe to %s: an administrator's "+
				"command would be published, retained and never carried out, "+
				"with a success shown in the console", want)
		}
	}
}

// TestAdminCommandPayloadsDecode pins the wire contract between the admin
// domain, which publishes these, and this one, which acts on them. Asserting
// on the JSON rather than on the struct is deliberate: a renamed tag on either
// side is exactly the change that would leave the id zero and the command a
// no-op.
func TestAdminCommandPayloadsDecode(t *testing.T) {
	appointmentID, keepID, adminID := uuid.New(), uuid.New(), uuid.New()

	t.Run("force cancel", func(t *testing.T) {
		env := envelope(t, events.SubjectAdminAppointmentForceCancel,
			events.AdminAppointmentForceCancelRequested{
				AppointmentID: appointmentID, Reason: "stuck consultation", AdminID: adminID,
			})

		var got events.AdminAppointmentForceCancelRequested
		if err := env.Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.AppointmentID != appointmentID {
			t.Errorf("appointment_id = %s, want %s", got.AppointmentID, appointmentID)
		}
		if got.AdminID != adminID {
			t.Errorf("admin_id = %s, want %s", got.AdminID, adminID)
		}
		if got.Reason != "stuck consultation" {
			t.Errorf("reason = %q, want %q", got.Reason, "stuck consultation")
		}
	})

	t.Run("resolve double booking", func(t *testing.T) {
		env := envelope(t, events.SubjectAdminDoubleBookingResolveRequested,
			events.AdminDoubleBookingResolveRequested{
				KeepAppointmentID:   keepID,
				CancelAppointmentID: appointmentID,
				Reason:              "double booked",
				AdminID:             adminID,
			})

		var got events.AdminDoubleBookingResolveRequested
		if err := env.Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The cancelled side is the one this service acts on. Confusing the
		// two would cancel the appointment the administrator chose to keep.
		if got.CancelAppointmentID != appointmentID {
			t.Errorf("cancel_appointment_id = %s, want %s", got.CancelAppointmentID, appointmentID)
		}
		if got.KeepAppointmentID != keepID {
			t.Errorf("keep_appointment_id = %s, want %s", got.KeepAppointmentID, keepID)
		}
	})

	t.Run("the two ids are distinct fields on the wire", func(t *testing.T) {
		raw, err := json.Marshal(events.AdminDoubleBookingResolveRequested{
			KeepAppointmentID: keepID, CancelAppointmentID: appointmentID,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, key := range []string{"keep_appointment_id", "cancel_appointment_id"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("payload has no %q field: %s", key, raw)
			}
		}
	})

	t.Run("consultation patient no-show", func(t *testing.T) {
		consultationID, appointmentID := uuid.New(), uuid.New()
		env := envelope(t, events.SubjectConsultationPatientNoShow,
			events.ConsultationPatientNoShow{
				ConsultationID: consultationID,
				AppointmentID:  appointmentID,
			})

		var got events.ConsultationPatientNoShow
		if err := env.Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.AppointmentID != appointmentID {
			t.Errorf("appointment_id = %s, want %s", got.AppointmentID, appointmentID)
		}
		if got.ConsultationID != consultationID {
			t.Errorf("consultation_id = %s, want %s", got.ConsultationID, consultationID)
		}
	})
}

func TestHandleConsultationPatientNoShow_UnparseableAcks(t *testing.T) {
	c := scheduling.NewConsumers(nil, nil, nil, zerolog.Nop())
	env := envelope(t, events.SubjectConsultationPatientNoShow, "not-json-object")
	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("unparseable consultation.patient_no_show must ack, got %v", err)
	}
}

func TestHandleConsultationPatientNoShow_MissingIDAcks(t *testing.T) {
	c := scheduling.NewConsumers(nil, nil, nil, zerolog.Nop())
	env := envelope(t, events.SubjectConsultationPatientNoShow, events.ConsultationPatientNoShow{})
	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("missing appointment_id must ack, got %v", err)
	}
}
