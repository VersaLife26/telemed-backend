// Sample e-prescription PDF for local preview (same renderer as production).
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"telemed/internal/domain/record/prescriptions"
)

// sampleSignature draws a rough scribble, standing in for a doctor's
// uploaded signature scan -- just enough that the sample PDF shows what a
// real one looks like sitting above the signature line, not a blank box.
func sampleSignature() []byte {
	w, h := 240, 80
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{})
		}
	}
	ink := color.RGBA{R: 20, G: 40, B: 110, A: 255}
	for x := 10; x < w-10; x++ {
		yf := float64(h)/2 + math.Sin(float64(x)/14)*18 + math.Sin(float64(x)/5)*6
		for dy := -2; dy <= 2; dy++ {
			y := int(yf) + dy
			if y >= 0 && y < h {
				img.Set(x, y, ink)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// sampleSeal draws a simple ring with a cross, standing in for a doctor's
// uploaded practice seal/stamp.
func sampleSeal() []byte {
	size := 160
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for x := range size {
		for y := range size {
			img.Set(x, y, color.RGBA{})
		}
	}
	ink := color.RGBA{R: 160, G: 20, B: 30, A: 220}
	cx, cy, r := float64(size)/2, float64(size)/2, float64(size)/2-8
	for angle := 0.0; angle < 360; angle += 0.25 {
		rad := angle * math.Pi / 180
		for _, ring := range []float64{r, r - 3} {
			x := int(cx + ring*math.Cos(rad))
			y := int(cy + ring*math.Sin(rad))
			if x >= 0 && x < size && y >= 0 && y < size {
				img.Set(x, y, ink)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func main() {
	out := filepath.Join("..", "..", "..", "sample-e-prescription.pdf")
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	p := prescriptions.Prescription{
		ID:                   uuid.MustParse("021e4b73-4338-4fb4-b771-44d84f20692f"),
		DoctorName:           "Nimal Perera",
		DoctorSLMC:           "SLMC12345",
		DoctorQualifications: "MBBS (Colombo), MD (ENT)\nUniversity: University of Colombo",
		IssuedAt:             time.Now().UTC().Truncate(time.Microsecond),
		Items: []prescriptions.Item{
			{
				DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule",
				Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21,
				Instructions: "Take after food", SortOrder: 0,
			},
			{
				DrugName: "Chlorpheniramine", Strength: "4mg", Form: "tablet",
				Dosage: "1 tablet", Frequency: "at night", DurationDays: 5, Quantity: 5,
				IsGeneric: true, SortOrder: 1,
			},
		},
	}
	secret := []byte("sample-secret-key-at-least-32-bytes!!")
	p.VerificationHMAC = prescriptions.Sign(secret, p)

	verifyURL := fmt.Sprintf("https://patient.versalifehealth.com/verify/prescriptions/%s?h=%s", p.ID, p.VerificationHMAC)
	patient := prescriptions.PatientDisplay{Name: "Lasana Pahanga", Age: 34}
	// Exactly "VersaLife Telemedicine" -- what the real frontend sends (see
	// issuePayload) -- so this sample matches production and demonstrates
	// the header's own de-duplication of a redundant clinic line, rather
	// than exercising a string real traffic never sends.
	clinic := prescriptions.ClinicDisplay{ClinicName: "VersaLife Telemedicine"}

	signature := &prescriptions.CredentialImage{Bytes: sampleSignature(), Kind: "PNG"}
	seal := &prescriptions.CredentialImage{Bytes: sampleSeal(), Kind: "PNG"}
	pdf, err := prescriptions.GeneratePDF(p, patient, clinic, verifyURL, signature, seal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate pdf: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, pdf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Println(out)
}
