package doctor

import (
	"errors"
	"strings"
	"testing"
)

func validApplyInput() ApplyInput {
	return ApplyInput{
		FirstName:           "Amila",
		LastName:            "Perera",
		Languages:           []Language{LanguageEN, LanguageSI},
		MedicalSchool:       "University of Colombo",
		QualificationsText:  "MBBS, MD",
		AvailabilityNotes:   "Weekdays 09:00–17:00",
		PracticingLocations: []string{"Nawaloka Hospital"},
		TermsAccepted:       true,
		Bank: &BankDetails{
			BankName:      "Commercial Bank",
			BranchName:    "Colombo 07",
			AccountNumber: "1234567890",
			AccountName:   "Amila Perera",
		},
	}
}

func TestValidateApplyInput_AcceptsACompleteApplication(t *testing.T) {
	t.Parallel()
	if err := validateApplyInput(validApplyInput()); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestValidateApplyInput_RequiresTermsAcceptance(t *testing.T) {
	t.Parallel()
	in := validApplyInput()
	in.TermsAccepted = false
	if err := validateApplyInput(in); !errors.Is(err, ErrTermsNotAccepted) {
		t.Fatalf("got %v, want ErrTermsNotAccepted", err)
	}
}

func TestValidateApplyInput_OtherLanguageNeedsAName(t *testing.T) {
	t.Parallel()
	in := validApplyInput()
	in.Languages = []Language{LanguageOther}
	in.LanguageOther = ""
	if err := validateApplyInput(in); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("got %v, want invalid transition for unnamed other language", err)
	}
	in.LanguageOther = "French"
	if err := validateApplyInput(in); err != nil {
		t.Fatalf("named other language rejected: %v", err)
	}
}

func TestValidateApplyInput_RequiresBankDetails(t *testing.T) {
	t.Parallel()
	in := validApplyInput()
	in.Bank = nil
	if err := validateApplyInput(in); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("nil bank: got %v", err)
	}
	in.Bank = &BankDetails{BankName: "Commercial Bank"}
	if err := validateApplyInput(in); err == nil || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("partial bank: got %v", err)
	}
}

func TestDocumentType_ValidOnApply(t *testing.T) {
	t.Parallel()
	for _, dt := range []DocumentType{DocumentSignature, DocumentSeal, DocumentSLMCCertificate} {
		if !dt.ValidOnApply() {
			t.Fatalf("%q must be accepted at apply time", dt)
		}
	}
	if DocumentNIC.ValidOnApply() {
		t.Fatal("NIC is not collected on the public apply form")
	}
}

func TestClipRunes(t *testing.T) {
	t.Parallel()
	if got := clipRunes("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", 201)
	got := clipRunes(long)
	if len([]rune(got)) != 200 {
		t.Fatalf("len=%d want 200 (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}
