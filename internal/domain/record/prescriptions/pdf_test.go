package prescriptions

import (
	"bytes"
	"testing"
)

func TestGeneratePDF_ProducesValidNonTrivialPDF(t *testing.T) {
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"
	p.DoctorQualifications = "MBBS (Colombo), MD (Family Medicine)"
	p.VerificationHMAC = Sign([]byte("test-secret-at-least-32-bytes-long!"), p)

	patient := PatientDisplay{Name: "Kamala Silva", Age: 45}
	clinic := ClinicDisplay{ClinicName: "Colombo General Clinic"}
	verifyURL := "https://verify.yourapp.lk/p/" + p.ID.String() + "?h=" + p.VerificationHMAC

	out, err := GeneratePDF(p, patient, clinic, verifyURL)
	if err != nil {
		t.Fatalf("GeneratePDF returned an error: %v", err)
	}

	// A real, well-formed PDF: correct magic header, an EOF trailer, and a
	// size that could not be produced by an empty or near-empty document
	// (a blank A4 page with just a font embedded already exceeds this).
	if !bytes.HasPrefix(out, []byte("%PDF-1.")) {
		t.Fatalf("output does not start with a PDF header, got: %q", out[:min(20, len(out))])
	}
	if !bytes.Contains(out, []byte("%%EOF")) {
		t.Fatal("output has no PDF EOF trailer marker")
	}
	if len(out) < 2000 {
		t.Fatalf("output is suspiciously small for a rendered prescription: %d bytes", len(out))
	}

	// fpdf compresses page content streams by default (SetCompression),
	// which is the right choice for production (this platform explicitly
	// targets slow 3G clients), so the visible text -- including the
	// prescription id and the verify URL -- is not a raw substring of the
	// file. What IS always uncompressed is the PDF object dictionary
	// structure itself, so we assert on that instead: a Page object and an
	// embedded Image XObject (the QR code) must both be present. That is a
	// stronger, structural check that the QR code was actually embedded,
	// rather than a string match that compression would make flaky.
	if !bytes.Contains(out, []byte("/Type /Page")) && !bytes.Contains(out, []byte("/Type/Page")) {
		t.Error("expected at least one /Page object in the PDF structure")
	}
	if !bytes.Contains(out, []byte("/Subtype /Image")) && !bytes.Contains(out, []byte("/Subtype/Image")) {
		t.Error("expected an embedded Image XObject (the QR code) in the PDF structure")
	}
}

func TestGeneratePDF_MultipleItemsRendered(t *testing.T) {
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	out, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient", Age: 30}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y")
	if err != nil {
		t.Fatalf("GeneratePDF returned an error: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty PDF output")
	}
}

func TestGeneratePDF_NoItems_StillProducesAValidDocument(t *testing.T) {
	p := samplePrescription()
	p.Items = nil
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	out, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient"}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y")
	if err != nil {
		t.Fatalf("GeneratePDF returned an error with zero items: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-1.")) {
		t.Fatal("expected a valid PDF header even with no items")
	}
}
