package prescriptions

import (
	"bytes"
	"fmt"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/skip2/go-qrcode"
)

// PatientDisplay carries the request-scoped patient details needed to render
// a human-readable PDF. This service does not own patient demographics
// (user-service does, ADR-004: no cross-service joins) and does not persist
// these fields anywhere -- they exist only for the duration of rendering
// this one PDF and are then discarded, which is why they are a parameter
// here rather than a column on Prescription.
type PatientDisplay struct {
	Name string
	Age  int
	NIC  string // optional, printed only if non-empty
}

// ClinicDisplay carries cosmetic header fields for the PDF that are not
// part of the signed/verified content.
type ClinicDisplay struct {
	ClinicName string
}

// GeneratePDF renders a complete, print-ready prescription: header (doctor
// name, SLMC number, qualifications), patient name and age, issue date, an
// Rx table of items, a signature block, a footer, and a QR code encoding the
// verification URL. It returns the raw PDF bytes.
func GeneratePDF(p Prescription, patient PatientDisplay, clinic ClinicDisplay, verifyURL string) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(18, 16, 18)
	pdf.AddPage()

	// --- header --------------------------------------------------------
	pdf.SetFont("Arial", "B", 16)
	pdf.CellFormat(0, 8, "E-Prescription", "", 1, "L", false, 0, "")
	if clinic.ClinicName != "" {
		pdf.SetFont("Arial", "", 11)
		pdf.CellFormat(0, 6, clinic.ClinicName, "", 1, "L", false, 0, "")
	}
	pdf.Ln(2)

	pdf.SetFont("Arial", "B", 12)
	pdf.CellFormat(0, 6, "Dr. "+p.DoctorName, "", 1, "L", false, 0, "")
	pdf.SetFont("Arial", "", 10)
	pdf.CellFormat(0, 5, "SLMC Registration No: "+p.DoctorSLMC, "", 1, "L", false, 0, "")
	if p.DoctorQualifications != "" {
		pdf.CellFormat(0, 5, p.DoctorQualifications, "", 1, "L", false, 0, "")
	}

	pdf.Ln(3)
	pdf.SetDrawColor(180, 180, 180)
	y := pdf.GetY()
	pdf.Line(18, y, 192, y)
	pdf.Ln(4)

	// --- patient + date --------------------------------------------------
	pdf.SetFont("Arial", "B", 10)
	pdf.CellFormat(95, 6, "Patient: "+patient.Name, "", 0, "L", false, 0, "")
	if patient.Age > 0 {
		pdf.CellFormat(0, 6, fmt.Sprintf("Age: %d", patient.Age), "", 1, "L", false, 0, "")
	} else {
		pdf.Ln(6)
	}
	pdf.SetFont("Arial", "", 10)
	pdf.CellFormat(95, 6, "Date: "+p.IssuedAt.UTC().Format("2006-01-02 15:04 MST"), "", 0, "L", false, 0, "")
	pdf.CellFormat(0, 6, "Prescription ID: "+p.ID.String(), "", 1, "L", false, 0, "")
	pdf.Ln(4)

	// --- Rx table --------------------------------------------------------
	pdf.SetFont("Arial", "B", 13)
	pdf.CellFormat(0, 7, "Rx", "", 1, "L", false, 0, "")

	pdf.SetFont("Arial", "B", 9)
	pdf.SetFillColor(235, 235, 235)
	widths := []float64{50, 22, 20, 32, 24, 16, 10}
	headers := []string{"Drug", "Strength", "Form", "Dosage / Frequency", "Duration", "Qty", "Gen."}
	for i, h := range headers {
		pdf.CellFormat(widths[i], 7, h, "1", 0, "C", true, 0, "")
	}
	pdf.Ln(-1)

	pdf.SetFont("Arial", "", 9)
	for i := range p.Items {
		it := &p.Items[i]
		generic := ""
		if it.IsGeneric {
			generic = "Yes"
		}
		rowHeight := 7.0
		pdf.CellFormat(widths[0], rowHeight, it.DrugName, "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[1], rowHeight, it.Strength, "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[2], rowHeight, it.Form, "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[3], rowHeight, it.Dosage+" / "+it.Frequency, "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[4], rowHeight, fmt.Sprintf("%d days", it.DurationDays), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[5], rowHeight, fmt.Sprintf("%d", it.Quantity), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[6], rowHeight, generic, "1", 1, "C", false, 0, "")
		if it.Instructions != "" {
			pdf.SetFont("Arial", "I", 8)
			pdf.CellFormat(0, 5, "  Note: "+it.Instructions, "", 1, "L", false, 0, "")
			pdf.SetFont("Arial", "", 9)
		}
	}

	pdf.Ln(10)

	// --- signature block ---------------------------------------------------
	sigY := pdf.GetY()
	pdf.Line(18, sigY+14, 78, sigY+14)
	pdf.SetXY(18, sigY+15)
	pdf.SetFont("Arial", "", 9)
	pdf.CellFormat(60, 5, "Doctor's Signature", "", 1, "L", false, 0, "")

	// --- QR code -------------------------------------------------------
	qrPNG, err := qrcode.Encode(verifyURL, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("prescriptions: generate qr code: %w", err)
	}
	const qrSize = 32.0
	qrX, qrY := 150.0, sigY
	pdf.RegisterImageOptionsReader("qr-verify", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG))
	pdf.ImageOptions("qr-verify", qrX, qrY, qrSize, qrSize, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	pdf.SetXY(qrX, qrY+qrSize+1)
	pdf.SetFont("Arial", "", 7)
	pdf.CellFormat(qrSize, 4, "Scan to verify", "", 1, "C", false, 0, "")

	// --- footer -------------------------------------------------------
	pdf.SetY(-20)
	pdf.SetFont("Arial", "I", 7)
	pdf.SetTextColor(120, 120, 120)
	pdf.CellFormat(0, 4, "This is a system-generated prescription. Verify authenticity at "+verifyURL, "", 1, "C", false, 0, "")
	pdf.CellFormat(0, 4, fmt.Sprintf("Generated %s | telemed-record-service", time.Now().UTC().Format(time.RFC3339)), "", 1, "C", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("prescriptions: render pdf: %w", err)
	}
	return buf.Bytes(), nil
}
