package prescriptions

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// tinyPNG returns a minimal, genuinely valid, solid-color PNG -- small
// enough to keep the test fast, but real enough that fpdf's image registry
// (and this package's own image.DecodeConfig sanity check in service.go)
// accept it exactly like a real signature or seal scan would be. Distinct
// colors matter: fpdf deduplicates embedded images by content hash, so two
// byte-identical images (e.g. a signature and seal both built from the same
// color) collapse into a single embedded object, which would make a test
// asserting "two images embedded" pass or fail for the wrong reason.
func tinyPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode tiny png: %v", err)
	}
	return buf.Bytes()
}

func TestGeneratePDF_ProducesValidNonTrivialPDF(t *testing.T) {
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"
	p.DoctorQualifications = "MBBS (Colombo), MD (Family Medicine)\nUniversity: University of Colombo"
	p.VerificationHMAC = Sign([]byte("test-secret-at-least-32-bytes-long!"), p)

	patient := PatientDisplay{Name: "Kamala Silva", Age: 45}
	clinic := ClinicDisplay{ClinicName: "Colombo General Clinic"}
	verifyURL := "https://verify.yourapp.lk/p/" + p.ID.String() + "?h=" + p.VerificationHMAC

	out, err := GeneratePDF(p, patient, clinic, verifyURL, nil, nil)
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
	// embedded Image XObject (the logo, since there is no signature/seal
	// here) must both be present.
	if !bytes.Contains(out, []byte("/Type /Page")) && !bytes.Contains(out, []byte("/Type/Page")) {
		t.Error("expected at least one /Page object in the PDF structure")
	}
	if !bytes.Contains(out, []byte("/Subtype /Image")) && !bytes.Contains(out, []byte("/Subtype/Image")) {
		t.Error("expected an embedded Image XObject (the logo and/or QR code) in the PDF structure")
	}
}

func TestGeneratePDF_MultipleItemsRendered(t *testing.T) {
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	out, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient", Age: 30}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y", nil, nil)
	if err != nil {
		t.Fatalf("GeneratePDF returned an error: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty PDF output")
	}
}

func TestGeneratePDF_OptionalPatientDetails(t *testing.T) {
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	cases := map[string]PatientDisplay{
		"all set": {Name: "Test Patient", Age: 30, Sex: "female", WeightKg: 62.5,
			Allergies: strings.Repeat("Penicillin, sulfa drugs, peanuts. ", 30)},
		"weight only": {Name: "Test Patient", WeightKg: 70},
		"none":        {Name: "Test Patient"},
	}
	for name, patient := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := GeneratePDF(p, patient, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y", nil, nil)
			if err != nil {
				t.Fatalf("GeneratePDF returned an error: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF-1.")) {
				t.Fatal("expected a valid PDF header")
			}
		})
	}
}

func TestGeneratePDF_NoItems_StillProducesAValidDocument(t *testing.T) {
	p := samplePrescription()
	p.Items = nil
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	out, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient"}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y", nil, nil)
	if err != nil {
		t.Fatalf("GeneratePDF returned an error with zero items: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-1.")) {
		t.Fatal("expected a valid PDF header even with no items")
	}
}

// TestGeneratePDF_WithSignatureAndSeal_EmbedsBothImages proves the doctor's
// actual signature and seal images are embedded as distinct image objects,
// not silently dropped -- the whole point of resolving them in Issue().
func TestGeneratePDF_WithSignatureAndSeal_EmbedsBothImages(t *testing.T) {
	signaturePNG := tinyPNG(t, color.RGBA{R: 20, G: 60, B: 120, A: 255})
	sealPNG := tinyPNG(t, color.RGBA{R: 160, G: 20, B: 30, A: 255})
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	withoutImages, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient", Age: 30}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y", nil, nil)
	if err != nil {
		t.Fatalf("GeneratePDF without images: %v", err)
	}
	withImages, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient", Age: 30}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y",
		&CredentialImage{Bytes: signaturePNG, Kind: "PNG"}, &CredentialImage{Bytes: sealPNG, Kind: "PNG"})
	if err != nil {
		t.Fatalf("GeneratePDF with images: %v", err)
	}

	countImages := func(pdf []byte) int {
		return bytes.Count(pdf, []byte("/Subtype /Image")) + bytes.Count(pdf, []byte("/Subtype/Image"))
	}
	got, want := countImages(withImages), countImages(withoutImages)+2
	if got < want {
		t.Fatalf("expected 2 more embedded images with a signature+seal than without (got %d, baseline %d)", got, countImages(withoutImages))
	}
	if !bytes.HasPrefix(withImages, []byte("%PDF-1.")) || !bytes.Contains(withImages, []byte("%%EOF")) {
		t.Fatal("expected a well-formed PDF even with two extra embedded images")
	}
}

// TestGeneratePDF_MissingSignatureOrSeal_StillRendersCleanly proves a nil
// signature or seal never breaks rendering -- Issue() only reaches
// GeneratePDF once both keys are known to exist, but loadCredentialImage can
// still hand back nil for a fetch or decode failure, and that must degrade
// to "no image", not a broken PDF.
func TestGeneratePDF_MissingSignatureOrSeal_StillRendersCleanly(t *testing.T) {
	somePNG := tinyPNG(t, color.RGBA{R: 20, G: 60, B: 120, A: 255})
	p := samplePrescription()
	p.DoctorName = "Nimal Perera"
	p.DoctorSLMC = "SLMC12345"

	cases := []struct {
		name      string
		signature *CredentialImage
		seal      *CredentialImage
	}{
		{"signature only", &CredentialImage{Bytes: somePNG, Kind: "PNG"}, nil},
		{"seal only", nil, &CredentialImage{Bytes: somePNG, Kind: "PNG"}},
		{"neither", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := GeneratePDF(p, PatientDisplay{Name: "Test Patient", Age: 30}, ClinicDisplay{}, "https://verify.yourapp.lk/p/x?h=y", tc.signature, tc.seal)
			if err != nil {
				t.Fatalf("GeneratePDF: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF-1.")) || !bytes.Contains(out, []byte("%%EOF")) {
				t.Fatal("expected a well-formed PDF")
			}
		})
	}
}
