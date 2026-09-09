package doctor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SECURITY-REVIEW F7.
//
// GET /doctors/{id}/reviews is auth: "public" in the gateway route table and
// served under OptionalAuth, and it used to put patient_id and appointment_id
// on every row. Walk the public GET /doctors search, then pull each doctor's
// reviews, and with no credentials at all you have a table of which patient
// consulted which doctor. For a psychiatrist, an HIV clinician or an
// obstetrician, the doctor's specialty IS the diagnosis -- so that table
// discloses medical condition on its own, and the ids then feed
// ?owner_user_id= against record-service.
//
// These tests assert against the SERIALISED BYTES, not against struct fields.
// A field can be renamed, embedded, or added to a shared struct; what reaches
// a client is the JSON, so that is what is checked.

// forbiddenOnPublicReview lists the JSON keys that must never appear on an
// unauthenticated review. It is a denylist checked against the marshalled
// output, so a field added to the type later is caught by name.
var forbiddenOnPublicReview = []string{"patient_id", "appointment_id", "patient", "user_id"}

func sampleReview() Review {
	return Review{
		ID:            uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		DoctorID:      uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		PatientID:     uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		AppointmentID: uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		Rating:        5,
		Comment:       "Very thorough, explained everything clearly.",
		CreatedAt:     time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC),
	}
}

func TestPublicReviewCarriesNoPatientIdentifier(t *testing.T) {
	t.Parallel()

	rv := sampleReview()
	raw, err := json.Marshal(toPublicReviewResponse(rv))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	// The value, not just the key: an id that leaked under a different name
	// would be exactly as useful to an attacker.
	if strings.Contains(body, rv.PatientID.String()) {
		t.Fatalf("the public review body contains the patient id.\nAnyone can now map patients to the "+
			"doctors they consulted, with no credentials.\nbody: %s", body)
	}
	if strings.Contains(body, rv.AppointmentID.String()) {
		t.Fatalf("the public review body contains the appointment id.\nbody: %s", body)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range forbiddenOnPublicReview {
		if _, present := decoded[key]; present {
			t.Fatalf("the public review body has a %q key; a public review needs a rating, a comment "+
				"and a date, and nothing that names the person who wrote it", key)
		}
	}

	// Positive control: it must still be a usable review. A DTO that dropped
	// everything would pass every assertion above and be worthless.
	if decoded["rating"] == nil || decoded["comment"] == nil || decoded["created_at"] == nil {
		t.Fatalf("the public review lost the fields it exists to carry: %s", body)
	}
	if decoded["comment"] != rv.Comment {
		t.Fatalf("comment = %v, want %q", decoded["comment"], rv.Comment)
	}
}

// The author of a review may see their own ids echoed back -- they sent them.
// This is here so that "strip the identifiers" does not quietly become "strip
// them everywhere", which would leave a client unable to tell which
// consultation it just reviewed.
func TestOwnReviewStillCarriesTheAuthorsOwnIdentifiers(t *testing.T) {
	t.Parallel()

	rv := sampleReview()
	raw, err := json.Marshal(toOwnReviewResponse(rv))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["patient_id"] != rv.PatientID.String() {
		t.Fatalf("patient_id = %v, want %s", decoded["patient_id"], rv.PatientID)
	}
	if decoded["appointment_id"] != rv.AppointmentID.String() {
		t.Fatalf("appointment_id = %v, want %s", decoded["appointment_id"], rv.AppointmentID)
	}
	if decoded["rating"] == nil {
		t.Fatal("the owner shape lost the public fields it embeds")
	}
}

// F7, second half. toDoctorResponse was shared between the public
// GET /doctors/{id} and the private GET /doctors/me, so rejection_reason -- an
// internal credentialing note about a named individual's professional
// competence -- was on the public profile of every doctor who had ever been
// rejected.
func TestPublicDoctorProfileCarriesNoInternalCredentialingNote(t *testing.T) {
	t.Parallel()

	d := Doctor{
		ID:                 uuid.New(),
		UserID:             uuid.New(),
		SLMCNumber:         "SLMC1234",
		Specialty:          "psychiatry",
		DisplayName:        "Dr. Example",
		VerificationStatus: StatusRejected,
		RejectionReason:    "SLMC certificate appears altered; registrar could not confirm the number",
	}

	raw, err := json.Marshal(toDoctorResponse(d))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	if strings.Contains(body, d.RejectionReason) {
		t.Fatalf("the public doctor profile contains the internal rejection reason.\nbody: %s", body)
	}
	if strings.Contains(body, "rejection_reason") {
		t.Fatalf("the public doctor profile has a rejection_reason key.\nbody: %s", body)
	}

	// The doctor themselves, and an admin reviewing them, must still see it --
	// otherwise a rejected applicant has no way to learn what to fix.
	own, err := json.Marshal(toOwnDoctorResponse(d))
	if err != nil {
		t.Fatalf("marshal own: %v", err)
	}
	if !strings.Contains(string(own), d.RejectionReason) {
		t.Fatalf("the doctor's own profile lost the rejection reason; a rejected applicant cannot act on "+
			"a decision they cannot read.\nbody: %s", own)
	}
}

// The public search result embeds the public doctor shape, so it inherits the
// fix -- but only as long as it keeps embedding the public one. This pins that.
func TestPublicSearchResultInheritsThePublicDoctorShape(t *testing.T) {
	t.Parallel()

	d := Doctor{
		ID: uuid.New(), UserID: uuid.New(), DisplayName: "Dr. Example",
		VerificationStatus: StatusRejected,
		RejectionReason:    "internal note that must not reach a patient",
	}

	raw, err := json.Marshal(toSearchResultResponse(SearchResult{Doctor: d}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), d.RejectionReason) {
		t.Fatalf("the public search result contains the internal rejection reason.\nbody: %s", raw)
	}
}
