package prescriptions

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/skip2/go-qrcode"
)

// versalifeLogoPNG is the VersaLife mark, rasterized once at build time from
// the same source as the web app's public/assets/logo.svg (a bundler can
// render an SVG; fpdf's image registry cannot, so this is a PNG instead of
// the vector original).
//
//go:embed assets/versalife-logo.png
var versalifeLogoPNG []byte

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

// CredentialImage is a decoded doctor signature or seal image, ready to
// embed on the PDF. A nil *CredentialImage anywhere GeneratePDF takes one
// means "not available" -- the layout renders around it (a blank signature
// line, no stamp) rather than failing, because a scan being briefly
// unreadable must never be the reason a patient does not get their
// prescription. Callers are expected to have already validated Bytes
// actually decodes as Kind (see prescriptions.Service.loadCredentialImage);
// this package trusts that and does not re-validate it.
type CredentialImage struct {
	Bytes []byte
	Kind  string // fpdf ImageType: "PNG" or "JPG"
}

// GeneratePDF renders a complete, print-ready prescription: a VersaLife
// header, the doctor's name/university/SLMC number/qualifications, patient
// name and age, issue date, an Rx table of items, a signature block (the
// doctor's actual signature and seal images when available), a footer, and
// a QR code encoding the verification URL. It returns the raw PDF bytes.
func GeneratePDF(p Prescription, patient PatientDisplay, clinic ClinicDisplay, verifyURL string,
	signature, seal *CredentialImage,
) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(18, 16, 18)
	pdf.AddPage()

	// --- header: logo + wordmark -----------------------------------------
	const headerTop, logoW = 14.0, 14.0
	logoH := logoW
	if info := pdf.RegisterImageOptionsReader("versalife-logo", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(versalifeLogoPNG)); info != nil && info.Width() > 0 {
		logoH = logoW * info.Height() / info.Width()
	}
	pdf.ImageOptions("versalife-logo", 18, headerTop, logoW, logoH, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")

	textX := 18 + logoW + 4
	pdf.SetXY(textX, headerTop)
	pdf.SetFont("Arial", "B", 15)
	pdf.CellFormat(0, 6, "VersaLife Telemedicine", "", 1, "L", false, 0, "")
	pdf.SetX(textX)
	pdf.SetFont("Arial", "", 10)
	pdf.SetTextColor(90, 90, 90)
	pdf.CellFormat(0, 5, "E-Prescription", "", 1, "L", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	// clinic.ClinicName is almost always "VersaLife Telemedicine" already
	// (see issuePayload in the frontend); repeating it under a header that
	// already says so is noise, not information.
	if clinic.ClinicName != "" && !strings.EqualFold(strings.TrimSpace(clinic.ClinicName), "VersaLife Telemedicine") {
		pdf.SetX(textX)
		pdf.SetFont("Arial", "", 9)
		pdf.CellFormat(0, 5, clinic.ClinicName, "", 1, "L", false, 0, "")
	}
	pdf.SetY(max(pdf.GetY(), headerTop+logoH) + 3)

	// --- doctor block ------------------------------------------------------
	pdf.SetFont("Arial", "B", 12)
	pdf.CellFormat(0, 6, "Dr. "+p.DoctorName, "", 1, "L", false, 0, "")
	pdf.SetFont("Arial", "", 10)
	pdf.CellFormat(0, 5, "SLMC Registration No: "+p.DoctorSLMC, "", 1, "L", false, 0, "")
	if p.DoctorQualifications != "" {
		// Sent as one or more newline-separated lines -- e.g. a degree line
		// and a "University: ..." line -- rather than one run-on sentence,
		// so the credentials block reads the way a printed prescription pad
		// does instead of a comma-separated database dump.
		for _, line := range strings.Split(p.DoctorQualifications, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			pdf.CellFormat(0, 5, line, "", 1, "L", false, 0, "")
		}
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
	// The doctor's actual signature image, if one was fetched, sits above
	// the line it would otherwise leave blank; the seal/stamp sits beside
	// it, slightly overlapping its right edge -- the same arrangement a
	// physical prescription pad has when a doctor signs and then presses a
	// rubber stamp next to the signature, not on top of the line itself.
	const sigBoxW, sigBoxH = 60.0, 18.0
	sigY := pdf.GetY()
	if signature != nil {
		drawFittedImage(pdf, "doctor-signature", signature, 18, sigY, sigBoxW, sigBoxH)
	}
	pdf.Line(18, sigY+sigBoxH, 18+sigBoxW, sigY+sigBoxH)
	pdf.SetXY(18, sigY+sigBoxH+1)
	pdf.SetFont("Arial", "", 9)
	pdf.CellFormat(sigBoxW, 5, "Doctor's Signature", "", 1, "L", false, 0, "")

	if seal != nil {
		const sealBoxW, sealBoxH = 26.0, 26.0
		drawFittedImage(pdf, "doctor-seal", seal, 18+sigBoxW+4, sigY-2, sealBoxW, sealBoxH)
	}

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

// drawFittedImage places img inside a maxW x maxH box anchored at (x, y),
// preserving its aspect ratio and centering it in the box rather than
// stretching a doctor's signature or seal scan into a shape it was never
// photographed in.
func drawFittedImage(pdf *fpdf.Fpdf, name string, img *CredentialImage, x, y, maxW, maxH float64) {
	info := pdf.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: img.Kind}, bytes.NewReader(img.Bytes))
	w, h := maxW, maxH
	if info != nil && info.Width() > 0 && info.Height() > 0 {
		ratio := info.Width() / info.Height()
		if maxW/ratio <= maxH {
			w, h = maxW, maxW/ratio
		} else {
			w, h = maxH*ratio, maxH
		}
	}
	ox, oy := x+(maxW-w)/2, y+(maxH-h)/2
	pdf.ImageOptions(name, ox, oy, w, h, false, fpdf.ImageOptions{ImageType: img.Kind}, 0, "")
}
