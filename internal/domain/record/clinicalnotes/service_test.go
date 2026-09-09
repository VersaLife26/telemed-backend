package clinicalnotes

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
)

// soapSample is deliberately the kind of text a real note contains: a
// specific, identifying, embarrassing sentence. Every "must not leak" test
// below searches for these exact strings, so a leak shows up as the sentence
// itself appearing where it should not.
const (
	soapSubjective = "Patient reports intermittent fever for 4 days, retro-orbital pain, no bleeding."
	soapObjective  = "Temp 38.9C, BP 100/60, platelets 92000, tourniquet test positive."
	soapAssessment = "Dengue fever with warning signs. Differential includes leptospirosis given paddy field exposure."
	soapPlan       = "Admit for monitoring. FBC 12-hourly. Avoid NSAIDs. Counselled on warning signs."
)

func doctorPrincipal(doctorID uuid.UUID) middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
}

// --- diagnosis normalisation -------------------------------------------

func TestNormaliseDiagnoses(t *testing.T) {
	tests := []struct {
		name      string
		in        []DiagnosisInput
		wantCodes []string
		wantErr   bool
	}{
		{"empty is fine", nil, nil, false},
		{
			"codes are upper-cased and trimmed",
			[]DiagnosisInput{{Code: " a90 "}, {Code: "e11.9"}},
			[]string{"A90", "E11.9"}, false,
		},
		{
			"one primary is allowed",
			[]DiagnosisInput{{Code: "A90", IsPrimary: true}, {Code: "E11.9"}},
			[]string{"A90", "E11.9"}, false,
		},
		{
			"two primaries are rejected",
			[]DiagnosisInput{{Code: "A90", IsPrimary: true}, {Code: "E11.9", IsPrimary: true}},
			nil, true,
		},
		{
			"a duplicate code is rejected even when cased differently",
			[]DiagnosisInput{{Code: "A90"}, {Code: "a90"}},
			nil, true,
		},
		{"an empty code is rejected", []DiagnosisInput{{Code: "   "}}, nil, true},
		{
			"more than MaxDiagnoses is rejected",
			make([]DiagnosisInput, MaxDiagnoses+1),
			nil, true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normaliseDiagnoses(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				assertUnprocessable(t, err)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.wantCodes) {
				t.Fatalf("got %d codes, want %d", len(got), len(tc.wantCodes))
			}
			for i, want := range tc.wantCodes {
				if got[i].Code != want {
					t.Errorf("code[%d] = %q, want %q", i, got[i].Code, want)
				}
			}
		})
	}
}

func assertUnprocessable(t *testing.T, err error) {
	t.Helper()
	apiErr := &httpx.APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *httpx.APIError", err)
	}
	if apiErr.Status() != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", apiErr.Status(), http.StatusUnprocessableEntity)
	}
}

// --- ICD-10 search query construction ----------------------------------

// TestBuildTSQuery covers the sanitisation that makes passing this string to
// to_tsquery safe. Every case where a tsquery operator could survive into the
// output is a case where a doctor's typo becomes a Postgres syntax error at
// best, and something worse at worst.
func TestBuildTSQuery(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"single word gets a prefix marker", "dengue", "dengue:*"},
		{"multiple words are ANDed", "dengue haem", "dengue:* & haem:*"},
		{"case is folded", "DENGUE Fever", "dengue:* & fever:*"},
		{"punctuation splits tokens", "gastro-oesophageal reflux", "gastro:* & oesophageal:* & reflux:*"},
		{"a code keeps its digits and drops the dot", "E11.9", "e11:* & 9:*"},
		{"tsquery operators cannot survive", "a & b | !c", "a:* & b:* & c:*"},
		{"a lone operator produces nothing", "&&& ||| !!!", ""},
		{"parentheses and colons are stripped", "fever:*(x)", "fever:* & x:*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTSQuery(tc.in); got != tc.want {
				t.Errorf("buildTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildTSQuery_NeverEmitsAnOperatorCharacter(t *testing.T) {
	// A property test over the shapes a doctor's keyboard can produce. The
	// output is interpolated into to_tsquery by Postgres, so a stray '&',
	// '|', '!', '(' or ')' is the whole risk.
	inputs := []string{
		"a&b", "a|b", "!a", "(a)", "a:b", "a<->b", "''; DROP TABLE icd10_codes; --",
		"\\", "*", "a:*&!b", "  ", "…dengue…",
	}
	for _, in := range inputs {
		got := buildTSQuery(in)
		for _, bad := range []string{"|", "!", "(", ")", "<", ">", "\\", "'", ";"} {
			if strings.Contains(got, bad) {
				t.Errorf("buildTSQuery(%q) = %q, contains forbidden %q", in, got, bad)
			}
		}
		// '&' and ':' appear only in the fixed " & " joiner and ":*" suffix.
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(got, " & ", ""), ":*", ""), "&") {
			t.Errorf("buildTSQuery(%q) = %q, contains a stray &", in, got)
		}
	}
}

func TestICD10CodePrefixRecognition(t *testing.T) {
	matches := []string{"A9", "A90", "E11", "E11.", "E11.9", "M54.5", "u07.1", "W54", "T63.0"}
	for _, m := range matches {
		if !icd10CodePrefix.MatchString(m) {
			t.Errorf("%q should be recognised as a code prefix", m)
		}
	}
	// A single letter would drag in a whole ICD chapter for someone who has
	// typed the first letter of a word; words obviously are not codes.
	nonMatches := []string{"E", "dengue", "fever", "a b", "1234", "E11.999", "", "diabetes"}
	for _, n := range nonMatches {
		if icd10CodePrefix.MatchString(n) {
			t.Errorf("%q should NOT be recognised as a code prefix", n)
		}
	}
}

// --- section validation -------------------------------------------------

func TestValidateSections(t *testing.T) {
	long := strings.Repeat("x", MaxSectionRunes+1)
	atLimit := strings.Repeat("x", MaxSectionRunes)

	if err := validateSections(atLimit, atLimit, atLimit, atLimit); err != nil {
		t.Fatalf("exactly at the limit must be accepted: %v", err)
	}

	for i, args := range [][4]string{
		{long, "", "", ""}, {"", long, "", ""}, {"", "", long, ""}, {"", "", "", long},
	} {
		err := validateSections(args[0], args[1], args[2], args[3])
		if err == nil {
			t.Fatalf("case %d: expected an error for an over-long section", i)
		}
		assertUnprocessable(t, err)
	}

	// Multi-byte content is counted in runes, not bytes: a note written in
	// Sinhala must not hit the limit three times sooner than one in English.
	sinhala := strings.Repeat("ජ", MaxSectionRunes)
	if err := validateSections(sinhala, "", "", ""); err != nil {
		t.Errorf("a Sinhala note at exactly the rune limit must be accepted: %v", err)
	}
}

// TestValidateSectionsErrorQuotesNoContent is a PHI test, not a formatting
// test. An error message is the single most likely thing to end up in a log
// line, a Sentry event and a support ticket, so it must name the field and
// never quote the field.
func TestValidateSectionsErrorQuotesNoContent(t *testing.T) {
	over := soapAssessment + strings.Repeat("x", MaxSectionRunes)
	err := validateSections("", "", over, "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "Dengue") || strings.Contains(err.Error(), soapAssessment[:20]) {
		t.Errorf("error message leaks note content: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "assessment") {
		t.Errorf("error message should name the offending section, got %q", err.Error())
	}
}

// --- content comparison (autosave idempotency) -------------------------

func TestNote_SameContentAs(t *testing.T) {
	base := Note{
		Subjective: soapSubjective, Objective: soapObjective,
		Assessment: soapAssessment, Plan: soapPlan,
		Diagnoses: []Diagnosis{{Code: "A90", Display: "Dengue fever", IsPrimary: true}},
	}

	tests := []struct {
		name   string
		mutate func(Note) Note
		want   bool
	}{
		{"identical", func(n Note) Note { return n }, true},
		{"subjective changed", func(n Note) Note { n.Subjective += "."; return n }, false},
		{"objective changed", func(n Note) Note { n.Objective = ""; return n }, false},
		{"assessment changed", func(n Note) Note { n.Assessment = "Viral fever"; return n }, false},
		{"plan changed", func(n Note) Note { n.Plan += " Review in 2 days."; return n }, false},
		{
			"a diagnosis added",
			func(n Note) Note {
				n.Diagnoses = append(append([]Diagnosis{}, n.Diagnoses...), Diagnosis{Code: "A27.9", Display: "Leptospirosis"})
				return n
			}, false,
		},
		{
			"primary flag moved",
			func(n Note) Note {
				n.Diagnoses = []Diagnosis{{Code: "A90", Display: "Dengue fever", IsPrimary: false}}
				return n
			}, false,
		},
		{
			"diagnoses reordered -- order is content, the doctor chose it",
			func(n Note) Note {
				n.Diagnoses = []Diagnosis{
					{Code: "A27.9", Display: "Leptospirosis"},
					{Code: "A90", Display: "Dengue fever", IsPrimary: true},
				}
				return n
			}, false,
		},
		{
			// Metadata is not content: a version bump from some other write
			// must not make an unchanged note look changed, or autosave would
			// write on every single call and never short-circuit.
			"version and timestamps differ but content does not",
			func(n Note) Note {
				n.Version = 99
				n.UpdatedAt = time.Now()
				n.ID = uuid.New()
				return n
			}, true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			other := tc.mutate(base)
			if got := base.SameContentAs(other); got != tc.want {
				t.Errorf("SameContentAs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNote_HasContent(t *testing.T) {
	if (Note{}).HasContent() {
		t.Error("an empty note must not report content")
	}
	if (Note{Plan: "   \n\t  "}).HasContent() {
		t.Error("whitespace is not content")
	}
	if !(Note{Objective: "Chest clear"}).HasContent() {
		t.Error("one populated section is content")
	}
}

func TestIsAuthor(t *testing.T) {
	doctorID := uuid.New()
	note := Note{DoctorID: doctorID, PatientID: uuid.New()}

	if !isAuthor(doctorPrincipal(doctorID), note) {
		t.Error("the note's own doctor is its author")
	}
	if isAuthor(doctorPrincipal(uuid.New()), note) {
		t.Error("a different doctor is not the author")
	}
	if isAuthor(middleware.Principal{UserID: note.PatientID, Roles: []middleware.Role{middleware.RolePatient}}, note) {
		t.Error("the patient is not the author")
	}
	// A doctor-role token with no doctor id claim must never match a note
	// whose doctor_id happens to be the zero UUID in a corrupt row.
	if isAuthor(middleware.Principal{Roles: []middleware.Role{middleware.RoleDoctor}}, Note{}) {
		t.Error("a principal with no DoctorID must never be treated as an author")
	}
	if isAuthor(middleware.Principal{}, note) {
		t.Error("an anonymous principal is not an author")
	}
}

// --- error translation --------------------------------------------------

// TestTranslate pins the status and CODE each failure surfaces as. The codes
// matter more than the statuses: CONFLICT tells a client to reload and
// retry, and NOTE_FINALISED tells it retrying will never work. A client that
// cannot tell them apart retries a finalised note forever.
func TestTranslate(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	appointmentID := uuid.New()

	tests := []struct {
		name       string
		in         error
		wantStatus int
		wantCode   httpx.ErrorCode
	}{
		{"optimistic lock", database.ErrOptimisticLock, http.StatusConflict, httpx.CodeConflict},
		{"wrapped optimistic lock", errors.Join(errors.New("outer"), database.ErrOptimisticLock), http.StatusConflict, httpx.CodeConflict},
		{"finalised", errFinalised, http.StatusConflict, httpx.CodeNoteFinalised},
		{"not finalised", errNotFinalised, http.StatusConflict, httpx.CodeConflict},
		{"no change", errNoChange, http.StatusUnprocessableEntity, httpx.CodeValidation},
		{"empty", errEmpty, http.StatusUnprocessableEntity, httpx.CodeValidation},
		{"not found", errNotFound, http.StatusNotFound, httpx.CodeNotFound},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, httpx.CodeInternal},
		{"an APIError passes through untouched", httpx.ErrForbidden, http.StatusForbidden, httpx.CodeForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.translate(tc.in, appointmentID, 0)
			apiErr := &httpx.APIError{}
			if !errors.As(got, &apiErr) {
				t.Fatalf("translate returned %v, not an *httpx.APIError", got)
			}
			if apiErr.Status() != tc.wantStatus {
				t.Errorf("status = %d, want %d", apiErr.Status(), tc.wantStatus)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", apiErr.Code, tc.wantCode)
			}
		})
	}
}

// TestTranslate_ConflictCarriesTheServerVersion checks the round-trip saver:
// two devices, one doctor, both mid-sentence. Handing back the version the
// server actually holds lets the losing client re-apply immediately instead
// of issuing another GET first.
func TestTranslate_ConflictCarriesTheServerVersion(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	err := svc.translate(database.ErrOptimisticLock, uuid.New(), 7)
	apiErr := &httpx.APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatal("not an APIError")
	}
	if apiErr.Fields["version"] != "7" {
		t.Errorf("fields[version] = %q, want \"7\"", apiErr.Fields["version"])
	}

	// With no known version there is nothing honest to report, so the field
	// is absent rather than "0" -- which a client would read as a real
	// version and send back.
	err = svc.translate(database.ErrOptimisticLock, uuid.New(), 0)
	_ = errors.As(err, &apiErr)
	if _, present := apiErr.Fields["version"]; present {
		t.Errorf("version must be omitted when unknown, got %v", apiErr.Fields)
	}
}

// --- PHI containment ----------------------------------------------------

// TestClinicalNoteFinalisedPayloadCannotCarryFreeText is structural, not
// behavioural, and that is the point: it fails if anyone ever ADDS a string
// field to the event -- the moment at which SOAP text or an ICD-10 code
// could start travelling on the bus. Asserting on one hand-built instance
// would not catch that; asserting on the type does.
func TestClinicalNoteFinalisedPayloadCannotCarryFreeText(t *testing.T) {
	typ := reflect.TypeOf(events.ClinicalNoteFinalised{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		switch f.Type.Kind() {
		case reflect.String, reflect.Map, reflect.Interface:
			t.Errorf("field %s is a %s: clinical_note.finalised carries identifiers only, "+
				"and a free-text field is how SOAP content or an ICD-10 code starts travelling on the event bus",
				f.Name, f.Type.Kind())
		case reflect.Slice:
			if f.Type.Elem().Kind() == reflect.String {
				t.Errorf("field %s is a []string: the event must not carry codes or text", f.Name)
			}
		}
	}
}

// TestFinalisedEventJSONContainsNoNoteContent builds the payload exactly as
// Finalise does and marshals it, then looks for the note's actual sentences
// and its diagnosis code in the bytes that would go to NATS.
func TestFinalisedEventJSONContainsNoNoteContent(t *testing.T) {
	payload := events.ClinicalNoteFinalised{
		NoteID: uuid.New(), AppointmentID: uuid.New(),
		PatientID: uuid.New(), DoctorID: uuid.New(),
		DiagnosisCount: 2, FinalisedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{soapSubjective, soapObjective, soapAssessment, soapPlan, "Dengue", "A90", "dengue"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("event payload contains %q: %s", forbidden, body)
		}
	}
}

// TestLoggerDeniesSOAPFieldNames is the guard the brief asks for: the shared
// denylist already covered "notes", "diagnosis" and "clinical_notes", none of
// which is what a structured log carrying a SOAP note would actually be
// keyed on. It would be keyed on the section names.
func TestLoggerDeniesSOAPFieldNames(t *testing.T) {
	fields := map[string]any{
		"subjective": soapSubjective,
		"objective":  soapObjective,
		"assessment": soapAssessment,
		"plan":       soapPlan,
		"diagnoses":  []string{"A90"},
		"soap":       soapSubjective,
		"note_id":    "9f1c...",
	}
	got := logger.Redact(fields)

	for _, k := range []string{"subjective", "objective", "assessment", "plan", "diagnoses", "soap"} {
		if got[k] != "[REDACTED]" {
			t.Errorf("field %q was not redacted: %v", k, got[k])
		}
	}
	// Redaction must not become a blanket: the identifiers that make a log
	// line useful have to survive, or operators stop logging altogether.
	if got["note_id"] != "9f1c..." {
		t.Errorf("note_id must survive redaction, got %v", got["note_id"])
	}

	// And nested: a request body logged whole is the classic accident.
	nested := logger.Redact(map[string]any{"body": map[string]any{"assessment": soapAssessment}})
	inner, ok := nested["body"].(map[string]any)
	if !ok {
		t.Fatalf("nested map was not preserved: %T", nested["body"])
	}
	if inner["assessment"] != "[REDACTED]" {
		t.Errorf("nested assessment was not redacted: %v", inner["assessment"])
	}
}

// TestAuthoriseWriteRejectsNonDoctorsWithoutTouchingTheDatabase proves the
// cheap rejections happen before any I/O: the Service here has a nil pool
// and nil access service, so anything that reached the database would panic.
func TestAuthoriseWriteRejectsNonDoctorsWithoutTouchingTheDatabase(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	for _, p := range []middleware.Principal{
		{},
		{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}},
		{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleSuperAdmin}},
		// A doctor-role token with no telemed_doctor_id claim: it cannot be
		// matched against a treating relationship, so it cannot write.
		{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}},
	} {
		_, err := svc.authoriseWrite(t.Context(), p, uuid.New())
		if err == nil {
			t.Fatalf("principal %v must be refused", p.Roles)
		}
		apiErr := &httpx.APIError{}
		if !errors.As(err, &apiErr) || apiErr.Status() != http.StatusForbidden {
			t.Errorf("principal %v: got %v, want 403", p.Roles, err)
		}
	}
}

func TestPrimaryRole(t *testing.T) {
	if got := primaryRole(middleware.Principal{}); got != "anonymous" {
		t.Errorf("primaryRole of an empty principal = %q, want \"anonymous\"", got)
	}
	if got := primaryRole(doctorPrincipal(uuid.New())); got != "doctor" {
		t.Errorf("primaryRole = %q, want \"doctor\"", got)
	}
}

func TestValueOr(t *testing.T) {
	s := "new"
	if got := valueOr(&s, "old"); got != "new" {
		t.Errorf("valueOr(&\"new\", \"old\") = %q", got)
	}
	if got := valueOr(nil, "old"); got != "old" {
		t.Errorf("valueOr(nil, \"old\") = %q", got)
	}
	// An amendment that explicitly blanks a section is a real edit and must
	// not be confused with an absent field.
	empty := ""
	if got := valueOr(&empty, "old"); got != "" {
		t.Errorf("an explicit empty string must clear the section, got %q", got)
	}
}

func TestSameCodesAndSameDiagnoses(t *testing.T) {
	stored := []Diagnosis{
		{Code: "A90", Display: "Dengue fever [classical dengue]", IsPrimary: true},
		{Code: "E11.9", Display: "Type 2 diabetes mellitus without complications"},
	}

	if !sameCodes(stored, []DiagnosisInput{{Code: "A90", IsPrimary: true}, {Code: "E11.9"}}) {
		t.Error("identical code sets should compare equal")
	}
	if sameCodes(stored, []DiagnosisInput{{Code: "E11.9"}, {Code: "A90", IsPrimary: true}}) {
		t.Error("reordered codes are a change")
	}
	if sameCodes(stored, []DiagnosisInput{{Code: "A90"}, {Code: "E11.9"}}) {
		t.Error("a moved primary flag is a change")
	}
	if sameCodes(stored, []DiagnosisInput{{Code: "A90", IsPrimary: true}}) {
		t.Error("a shorter list is a change")
	}

	if !sameDiagnoses(stored, stored) {
		t.Error("a list should equal itself")
	}
	relabelled := []Diagnosis{{Code: "A90", Display: "Something else", IsPrimary: true}, stored[1]}
	if sameDiagnoses(stored, relabelled) {
		t.Error("a changed display term is a change -- it is what appears on the record")
	}
}
