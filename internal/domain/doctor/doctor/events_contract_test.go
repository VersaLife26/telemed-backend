package doctor

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// Golden wire format for doctor.approved.
//
// This test pins BYTES, not a struct. A struct-to-struct assertion cannot
// catch a renamed json tag, because both sides move together -- and a renamed
// tag is exactly how scheduling-service would stop seeing a fee while every
// Go test in this repo stayed green.
//
// If this test fails, the wire contract changed. That is allowed, but it is a
// cross-repo change: per the versioning rule in events/payloads.go, adding an
// optional field is safe and renaming or removing one requires a new subject.
// Update the golden only after confirming which of those you did.
const goldenDoctorApproved = `{` +
	`"doctor_id":"11111111-1111-4111-8111-111111111111",` +
	`"user_id":"22222222-2222-4222-8222-222222222222",` +
	`"doctor_name":"Dr Anula Perera",` +
	`"specialty":"cardiology",` +
	`"fee_cents":250000,` +
	`"currency":"LKR",` +
	`"languages":["en","si"],` +
	// working_hours is what makes the doctor bookable at all. Its absence
	// meant scheduling generated zero slots for every approved doctor.
	`"working_hours":[{"day_of_week":1,"start_time":"09:00","end_time":"12:00","is_available":true},{"day_of_week":2,"start_time":"09:00","end_time":"12:00","is_available":true},{"day_of_week":3,"start_time":"09:00","end_time":"12:00","is_available":true},{"day_of_week":4,"start_time":"09:00","end_time":"12:00","is_available":true},{"day_of_week":5,"start_time":"09:00","end_time":"12:00","is_available":true}],` +
	`"timezone":"Asia/Colombo",` +
	// The slot shape. Without these scheduling-service slices the doctor's
	// windows with ITS defaults, not the doctor's declaration.
	`"slot_duration_minutes":20,` +
	// buffer_minutes is 0 and PRESENT. A pointer-to-zero means "back-to-back,
	// no gap", which is a preference; nil means "none expressed" and is
	// omitted entirely (TestApprovedPayloadOmitsAnUnsetBuffer). If this ever
	// renders as an absent field for a doctor who asked for 0, they silently
	// get the 5-minute default forever.
	`"buffer_minutes":0,` +
	`"max_per_day":12,` +
	`"approved_at":"2026-08-20T09:30:00Z"` +
	`}`

// fixtureSettings is the slot-shape declaration the golden payload carries.
func fixtureSettings() ScheduleSettings {
	zero := 0
	return ScheduleSettings{
		DoctorID:            fixtureDoctor().ID,
		SlotDurationMinutes: 20,
		BufferMinutes:       &zero,
		MaxPerDay:           12,
		Timezone:            DefaultScheduleTimezone,
	}
}

func fixtureDoctor() Doctor {
	return Doctor{
		ID:                 uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		UserID:             uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		DisplayName:        "Dr Anula Perera",
		Specialty:          "cardiology",
		FeeCents:           250000, // LKR 2,500.00
		Currency:           "LKR",
		Languages:          []Language{LanguageEN, LanguageSI},
		VerificationStatus: StatusApproved,
	}
}

// fixtureWorkingHours is the weekly pattern the golden payload carries.
func fixtureWorkingHours() []WorkingHour {
	var out []WorkingHour
	for day := 1; day <= 5; day++ {
		out = append(out, WorkingHour{
			DoctorID: fixtureDoctor().ID, DayOfWeek: day,
			StartTime: "09:00:00", EndTime: "12:00:00", IsAvailable: true,
		})
	}
	return out
}

func TestDoctorApprovedGoldenWireFormat(t *testing.T) {
	at := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)

	raw, err := json.Marshal(approvedPayload(fixtureDoctor(), fixtureWorkingHours(), fixtureSettings(), at))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != goldenDoctorApproved {
		t.Errorf("doctor.approved wire format drifted.\n got: %s\nwant: %s", raw, goldenDoctorApproved)
	}

	// And the decode side: the field scheduling-service prices off must survive
	// a round trip through the wire with a non-zero value.
	var back events.DoctorApproved
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.FeeCents != 250000 {
		t.Errorf("fee_cents = %d, want 250000 -- this is the field whose absence "+
			"meant no patient could pay", back.FeeCents)
	}
	// Working hours are what make the doctor bookable at all. Without them
	// scheduling generates zero slots and the doctor is approved but
	// permanently unbookable, with nothing logged to say why.
	if len(back.WorkingHours) != 5 {
		t.Errorf("working_hours has %d entries, want 5 -- an approved doctor "+
			"with no hours can never be booked", len(back.WorkingHours))
	}
	if len(back.WorkingHours) > 0 {
		if got := back.WorkingHours[0].StartTime; got != "09:00" {
			t.Errorf("start_time = %q, want %q (HH:MM, seconds trimmed)", got, "09:00")
		}
	}
	if back.Timezone != "Asia/Colombo" {
		t.Errorf("timezone = %q, want Asia/Colombo", back.Timezone)
	}
	if back.Currency != "LKR" {
		t.Errorf("currency = %q, want LKR", back.Currency)
	}
	if back.Specialty != "cardiology" {
		t.Errorf("specialty = %q, want cardiology", back.Specialty)
	}
}

// TestApprovedPayloadCarriesEverythingSchedulingNeeds is the regression test
// for INTEGRATION-FIXES #10. The old private payload sent {doctor_id, user_id}
// and nothing else.
func TestApprovedPayloadCarriesEverythingSchedulingNeeds(t *testing.T) {
	got := approvedPayload(fixtureDoctor(), fixtureWorkingHours(), fixtureSettings(), time.Now().UTC())

	switch {
	case got.DoctorID == uuid.Nil:
		t.Error("doctor_id is empty")
	case got.FeeCents <= 0:
		t.Error("fee_cents is zero: scheduling cannot quote a price and the " +
			"booking becomes unpayable")
	case got.Currency == "":
		t.Error("currency is empty")
	case got.Specialty == "":
		t.Error("specialty is empty: payment prices its commission off it")
	case got.DoctorName == "":
		t.Error("doctor_name is empty: notification addresses the approval to nobody")
	case got.ApprovedAt.IsZero():
		t.Error("approved_at is zero")
	}

	// Email is knowingly absent -- see approvedPayload's comment. Asserted so
	// that "we have no email" stays a deliberate, documented state rather than
	// something a future reader has to rediscover.
	if got.Email != "" {
		t.Errorf("email = %q: doctor-service has no email column, so a non-empty "+
			"value here means somebody invented a source", got.Email)
	}
}

func TestDefaultCurrencyFillsBlank(t *testing.T) {
	d := fixtureDoctor()
	d.Currency = ""
	if got := approvedPayload(d, fixtureWorkingHours(), fixtureSettings(), time.Now().UTC()).Currency; got != DefaultCurrency {
		t.Errorf("currency = %q, want %q for a row written before the column existed", got, DefaultCurrency)
	}
}

func TestDoctorUpdatedCarriesPricing(t *testing.T) {
	d := fixtureDoctor()
	d.FeeCents = 300000
	got := updatedPayload(d, fixtureWorkingHours(), fixtureSettings(), time.Now().UTC())

	if got.FeeCents != 300000 {
		t.Errorf("fee_cents = %d, want 300000", got.FeeCents)
	}
	if got.Status != string(StatusApproved) {
		t.Errorf("status = %q, want approved", got.Status)
	}
	if got.DoctorID != d.ID {
		t.Errorf("doctor_id = %s, want %s", got.DoctorID, d.ID)
	}
}

func TestPricingChanged(t *testing.T) {
	base := fixtureDoctor()

	tests := []struct {
		name   string
		mutate func(*Doctor)
		want   bool
	}{
		{"fee raised", func(d *Doctor) { d.FeeCents = 300000 }, true},
		{"specialty changed", func(d *Doctor) { d.Specialty = "dermatology" }, true},
		{"currency changed", func(d *Doctor) { d.Currency = "USD" }, true},
		{"status changed", func(d *Doctor) { d.VerificationStatus = StatusSuspended }, true},
		{"language added", func(d *Doctor) { d.Languages = append(d.Languages, LanguageTA) }, true},
		{"language removed", func(d *Doctor) { d.Languages = []Language{LanguageEN} }, true},
		{"languages reordered", func(d *Doctor) { d.Languages = []Language{LanguageSI, LanguageEN} }, false},
		{"bio rewritten", func(d *Doctor) { d.Bio = "twenty years in Colombo" }, false},
		{"photo replaced", func(d *Doctor) { d.PhotoURL = "https://cdn.example/x.jpg" }, false},
		{"nothing", func(*Doctor) {}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			after := base
			after.Languages = append([]Language(nil), base.Languages...)
			tt.mutate(&after)
			if got := pricingChanged(base, after); got != tt.want {
				t.Errorf("pricingChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// doctor.registered / doctor.documents_updated
// ---------------------------------------------------------------------------

// Golden wire format for doctor.registered.
//
// The old private payload sent four fields -- doctor_id, user_id, slmc_number,
// specialty -- against the eleven admin-service's verification queue declared.
// Every application therefore projected as
// {"full_name":"","years_experience":0,"registered_at":"0001-01-01"} with no
// attachments, which is what a reviewer actually saw on the flagship admin
// screen. Same rule as the doctor.approved golden above: this pins BYTES,
// because a renamed json tag moves both Go structs together and would leave
// every test in this repo green while the queue went blank again.
const goldenDoctorRegistered = `{` +
	`"doctor_id":"11111111-1111-4111-8111-111111111111",` +
	`"user_id":"22222222-2222-4222-8222-222222222222",` +
	`"full_name":"Dr Anula Perera",` +
	`"slmc_number":"SLMC/12345",` +
	`"specialty":"cardiology",` +
	`"years_experience":12,` +
	`"slmc_certificate_key":"doctors/11111111/slmc.pdf",` +
	`"nic_document_key":"doctors/11111111/nic.jpg",` +
	`"degree_certificate_key":"doctors/11111111/degree.pdf",` +
	`"photo_key":"doctors/11111111/photo.jpg",` +
	`"created_at":"2026-08-20T09:30:00Z"` +
	`}`

func fixtureDocuments() []Document {
	id := fixtureDoctor().ID
	// Newest first, exactly as listDocuments orders them.
	return []Document{
		{ID: uuid.New(), DoctorID: id, DocumentType: DocumentPhoto, ObjectKey: "doctors/11111111/photo.jpg"},
		{ID: uuid.New(), DoctorID: id, DocumentType: DocumentDegreeCertificate, ObjectKey: "doctors/11111111/degree.pdf"},
		{ID: uuid.New(), DoctorID: id, DocumentType: DocumentNIC, ObjectKey: "doctors/11111111/nic.jpg"},
		{ID: uuid.New(), DoctorID: id, DocumentType: DocumentSLMCCertificate, ObjectKey: "doctors/11111111/slmc.pdf"},
	}
}

func TestDoctorRegisteredGoldenWireFormat(t *testing.T) {
	d := fixtureDoctor()
	d.SLMCNumber = "SLMC/12345"
	d.ExperienceYears = 12
	at := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)

	raw, err := json.Marshal(registeredPayload(d, CredentialDocumentKeys(fixtureDocuments()), at))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != goldenDoctorRegistered {
		t.Errorf("doctor.registered wire format drifted.\n got: %s\nwant: %s", raw, goldenDoctorRegistered)
	}
}

// TestRegisteredPayloadCarriesWhatTheReviewQueueNeeds is the regression test
// for the blank credentialing queue. Each field here maps to a column
// admin-service's doctor_projection already had and nothing ever filled.
func TestRegisteredPayloadCarriesWhatTheReviewQueueNeeds(t *testing.T) {
	d := fixtureDoctor()
	d.SLMCNumber = "SLMC/12345"
	d.ExperienceYears = 12
	got := registeredPayload(d, CredentialDocumentKeys(fixtureDocuments()), time.Now().UTC())

	if got.FullName == "" {
		t.Error("full_name is empty: the queue row renders nameless")
	}
	if got.YearsExperience == 0 {
		t.Error("years_experience is zero: the reviewer's experience check has nothing to check")
	}
	if got.SLMCNumber == "" {
		t.Error("slmc_number is empty: the registry check has nothing to look up")
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero: every application sorts as 0001-01-01 and the queue " +
			"cannot be ordered oldest-first")
	}
	if got.Empty() {
		t.Error("no credential document keys: the reviewer sees an application with no " +
			"attachments, which is the whole reason this event fans out")
	}
	for name, key := range map[string]string{
		"slmc_certificate_key":   got.SLMCCertificateKey,
		"nic_document_key":       got.NICDocumentKey,
		"degree_certificate_key": got.DegreeCertificateKey,
		"photo_key":              got.PhotoKey,
	} {
		if key == "" {
			t.Errorf("%s is empty", name)
		}
	}
}

// TestRegisteredPayloadWithNoDocumentsOmitsTheKeys pins the normal case: a
// doctor registers before uploading anything, so the keys are absent from the
// wire rather than present-and-empty. Absent is what lets a consumer tell
// "nothing submitted yet" from "submitted, and the producer lost it".
func TestRegisteredPayloadWithNoDocumentsOmitsTheKeys(t *testing.T) {
	raw, err := json.Marshal(registeredPayload(fixtureDoctor(), CredentialDocumentKeys(nil), time.Now().UTC()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"slmc_certificate_key", "nic_document_key", "degree_certificate_key", "photo_key"} {
		if bytes.Contains(raw, []byte(key)) {
			t.Errorf("%s should be omitted when no document has been uploaded, got: %s", key, raw)
		}
	}
}

func TestCredentialDocumentKeys(t *testing.T) {
	id := fixtureDoctor().ID

	t.Run("newest upload of a type wins", func(t *testing.T) {
		// listDocuments orders newest first, so the replacement leads.
		got := CredentialDocumentKeys([]Document{
			{DoctorID: id, DocumentType: DocumentNIC, ObjectKey: "new.jpg"},
			{DoctorID: id, DocumentType: DocumentNIC, ObjectKey: "old.jpg"},
		})
		if got.NICDocumentKey != "new.jpg" {
			t.Errorf("nic_document_key = %q, want new.jpg -- a re-upload after a rejection "+
				"must be what the reviewer opens", got.NICDocumentKey)
		}
	})

	t.Run("a board certificate is not a degree certificate", func(t *testing.T) {
		got := CredentialDocumentKeys([]Document{
			{DoctorID: id, DocumentType: DocumentSpecialtyBoardCert, ObjectKey: "board.pdf"},
			{DoctorID: id, DocumentType: DocumentOther, ObjectKey: "misc.pdf"},
		})
		if !got.Empty() {
			t.Errorf("types the queue does not review must not be folded into one that it "+
				"does; got %+v", got)
		}
	})

	t.Run("no documents is empty, not a zero-valued key", func(t *testing.T) {
		if !CredentialDocumentKeys(nil).Empty() {
			t.Error("Empty() should report true for a doctor who has uploaded nothing")
		}
	})
}

func TestDocumentsUpdatedCarriesTheFullSet(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	got := documentsUpdatedPayload(fixtureDoctor(), CredentialDocumentKeys(fixtureDocuments()), at)

	if got.DoctorID != fixtureDoctor().ID {
		t.Errorf("doctor_id = %s, want %s", got.DoctorID, fixtureDoctor().ID)
	}
	if got.UserID != fixtureDoctor().UserID {
		t.Errorf("user_id = %s, want %s", got.UserID, fixtureDoctor().UserID)
	}
	if got.UpdatedAt != at {
		t.Errorf("updated_at = %s, want %s", got.UpdatedAt, at)
	}
	// The full set, not a delta: a consumer that missed the previous three
	// uploads is still correct after this one event.
	if got.Empty() || got.SLMCCertificateKey == "" || got.NICDocumentKey == "" ||
		got.DegreeCertificateKey == "" || got.PhotoKey == "" {
		t.Errorf("documents_updated must carry every current key, got %+v", got.DoctorCredentialDocuments)
	}

	// And it decodes into the canonical type both services share.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back events.DoctorDocumentsUpdated
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PhotoKey != "doctors/11111111/photo.jpg" {
		t.Errorf("photo_key = %q after a round trip", back.PhotoKey)
	}
}

// TestPhotoIsAValidDocumentType guards the mapping between what a doctor may
// upload and what the admin checklist asks a reviewer to confirm. The
// checklist has a photo_clear item and the projection a photo_key column; if
// `photo` stops being an accepted document type there is no way to submit the
// thing the reviewer must sign off on.
func TestPhotoIsAValidDocumentType(t *testing.T) {
	for _, dt := range []DocumentType{
		DocumentSLMCCertificate, DocumentNIC, DocumentDegreeCertificate, DocumentPhoto,
	} {
		if !dt.Valid() {
			t.Errorf("%q must be an accepted document type: the verification queue reviews it", dt)
		}
	}
	if DocumentType("passport").Valid() {
		t.Error("an unknown document type must be rejected")
	}
}
