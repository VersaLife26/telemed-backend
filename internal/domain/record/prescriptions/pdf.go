package prescriptions

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

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

// Brand colours from the VersaLife mark (public/assets/logo.svg).
const (
	brandNavyR, brandNavyG, brandNavyB = 1, 85, 145   // #015591
	brandMintR, brandMintG, brandMintB = 80, 200, 152 // #50C898
	pdfLeft                            = 16.0
	pdfRight                           = 194.0
)

// GeneratePDF renders a print-ready VersaLife prescription: logo and
// wordmark, the doctor's name, university, and SLMC number, the patient's
// name and age, the medicines, then the doctor's signature and stamp with
// a verification QR. It returns the raw PDF bytes.
func GeneratePDF(p Prescription, patient PatientDisplay, clinic ClinicDisplay, verifyURL string,
	signature, seal *CredentialImage,
) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(pdfLeft, 14, 16)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()

	contentW := pdfRight - pdfLeft

	// --- header: logo, wordmark, document title --------------------------
	const headerTop, logoW = 12.0, 16.0
	logoH := logoW
	if info := pdf.RegisterImageOptionsReader("versalife-logo", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(versalifeLogoPNG)); info != nil && info.Width() > 0 {
		logoH = logoW * info.Height() / info.Width()
	}
	pdf.ImageOptions("versalife-logo", pdfLeft, headerTop, logoW, logoH, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")

	textX := pdfLeft + logoW + 4
	pdf.SetXY(textX, headerTop+1)
	pdf.SetFont("Arial", "B", 16)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(90, 7, "VersaLife", "", 1, "L", false, 0, "")
	pdf.SetX(textX)
	pdf.SetFont("Arial", "", 9)
	pdf.SetTextColor(70, 70, 70)
	pdf.CellFormat(90, 4, "Telemedicine", "", 1, "L", false, 0, "")

	pdf.SetXY(120, headerTop+2)
	pdf.SetFont("Arial", "B", 11)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(pdfRight-120, 6, "E-PRESCRIPTION", "", 1, "R", false, 0, "")
	pdf.SetX(120)
	pdf.SetFont("Arial", "", 8)
	pdf.SetTextColor(90, 90, 90)
	subtitle := "Sri Lanka"
	if clinic.ClinicName != "" && !strings.EqualFold(strings.TrimSpace(clinic.ClinicName), "VersaLife Telemedicine") {
		subtitle = pdfSafe(clinic.ClinicName)
	}
	pdf.CellFormat(pdfRight-120, 4, subtitle, "", 1, "R", false, 0, "")

	barY := max(headerTop+logoH, pdf.GetY()) + 3
	pdf.SetFillColor(brandMintR, brandMintG, brandMintB)
	pdf.Rect(pdfLeft, barY, contentW, 1.6, "F")
	pdf.SetY(barY + 6)

	// --- doctor: name, degree, university, SLMC --------------------------
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.SetFont("Arial", "B", 13)
	pdf.CellFormat(contentW, 7, "Dr. "+pdfDoctorName(p.DoctorName), "", 1, "L", false, 0, "")
	pdf.SetTextColor(40, 40, 40)
	pdf.SetFont("Arial", "", 10)
	for _, line := range strings.Split(p.DoctorQualifications, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pdf.MultiCell(contentW, 5, pdfSafe(line), "", "L", false)
	}
	pdf.SetFont("Arial", "B", 10)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(contentW, 6, "SLMC Registration No. "+pdfSafe(p.DoctorSLMC), "", 1, "L", false, 0, "")

	// --- patient card ------------------------------------------------------
	pdf.Ln(3)
	cardY := pdf.GetY()
	const cardH = 16.0
	pdf.SetFillColor(232, 242, 250)
	pdf.Rect(pdfLeft, cardY, contentW, cardH, "F")
	pdf.SetXY(pdfLeft+4, cardY+2)
	pdf.SetFont("Arial", "", 7)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(100, 4, "PATIENT", "", 0, "L", false, 0, "")
	pdf.CellFormat(30, 4, "AGE", "", 0, "L", false, 0, "")
	pdf.CellFormat(0, 4, "DATE", "", 1, "L", false, 0, "")
	pdf.SetX(pdfLeft + 4)
	pdf.SetFont("Arial", "B", 11)
	pdf.SetTextColor(20, 20, 20)
	age := "-"
	if patient.Age > 0 {
		age = fmt.Sprintf("%d years", patient.Age)
	}
	pdf.CellFormat(100, 6, pdfSafe(patient.Name), "", 0, "L", false, 0, "")
	pdf.SetFont("Arial", "", 11)
	pdf.CellFormat(30, 6, age, "", 0, "L", false, 0, "")
	pdf.CellFormat(0, 6, p.IssuedAt.UTC().Format("02 Jan 2006"), "", 1, "L", false, 0, "")
	pdf.SetY(cardY + cardH + 6)

	// --- medicines ---------------------------------------------------------
	pdf.SetFont("Arial", "B", 14)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(contentW, 8, "Rx", "", 1, "L", false, 0, "")

	widths := []float64{48, 22, 20, 36, 22, 16, 14}
	headers := []string{"Medicine", "Strength", "Form", "Dosage / Frequency", "Duration", "Qty", "Generic"}
	pdf.SetFillColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Arial", "B", 7)
	for i, h := range headers {
		pdf.CellFormat(widths[i], 7, h, "", 0, "C", true, 0, "")
	}
	pdf.Ln(-1)

	pdf.SetTextColor(30, 30, 30)
	pdf.SetFont("Arial", "", 9)
	pdf.SetDrawColor(210, 220, 230)
	for i := range p.Items {
		it := &p.Items[i]
		if i%2 == 0 {
			pdf.SetFillColor(245, 248, 252)
		} else {
			pdf.SetFillColor(255, 255, 255)
		}
		generic := ""
		if it.IsGeneric {
			generic = "Yes"
		}
		dose := strings.Trim(pdfSafe(it.Dosage)+" / "+pdfSafe(it.Frequency), " /")
		const rowH = 7.0
		pdf.CellFormat(widths[0], rowH, pdfSafe(it.DrugName), "B", 0, "L", true, 0, "")
		pdf.CellFormat(widths[1], rowH, pdfSafe(it.Strength), "B", 0, "C", true, 0, "")
		pdf.CellFormat(widths[2], rowH, pdfSafe(it.Form), "B", 0, "C", true, 0, "")
		pdf.CellFormat(widths[3], rowH, dose, "B", 0, "L", true, 0, "")
		pdf.CellFormat(widths[4], rowH, fmt.Sprintf("%d days", it.DurationDays), "B", 0, "C", true, 0, "")
		pdf.CellFormat(widths[5], rowH, fmt.Sprintf("%d", it.Quantity), "B", 0, "C", true, 0, "")
		pdf.CellFormat(widths[6], rowH, generic, "B", 1, "C", true, 0, "")
		if it.Instructions != "" {
			pdf.SetFont("Arial", "I", 8)
			pdf.SetTextColor(80, 80, 80)
			pdf.CellFormat(contentW, 5, "    "+pdfSafe(it.Instructions), "", 1, "L", false, 0, "")
			pdf.SetFont("Arial", "", 9)
			pdf.SetTextColor(30, 30, 30)
		}
	}

	// --- signature, stamp, QR, pinned toward the foot of the page --------
	const sigBoxW, sigBoxH = 62.0, 20.0
	sigY := pdf.GetY() + 12
	if sigY < 205 {
		sigY = 205
	}
	if sigY > 230 {
		pdf.AddPage()
		sigY = 40
	}
	if signature != nil {
		drawFittedImage(pdf, "doctor-signature", signature, pdfLeft, sigY, sigBoxW, sigBoxH)
	}
	pdf.SetDrawColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.SetLineWidth(0.3)
	pdf.Line(pdfLeft, sigY+sigBoxH, pdfLeft+sigBoxW, sigY+sigBoxH)
	pdf.SetXY(pdfLeft, sigY+sigBoxH+1.5)
	pdf.SetFont("Arial", "", 8)
	pdf.SetTextColor(60, 60, 60)
	pdf.CellFormat(sigBoxW, 4, "Doctor's signature", "", 1, "L", false, 0, "")
	pdf.SetX(pdfLeft)
	pdf.SetFont("Arial", "B", 8)
	pdf.SetTextColor(brandNavyR, brandNavyG, brandNavyB)
	pdf.CellFormat(sigBoxW, 4, "Dr. "+pdfDoctorName(p.DoctorName), "", 1, "L", false, 0, "")

	if seal != nil {
		drawFittedImage(pdf, "doctor-seal", seal, pdfLeft+sigBoxW+8, sigY-2, 28, 28)
		pdf.SetXY(pdfLeft+sigBoxW+8, sigY+26)
		pdf.SetFont("Arial", "", 7)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(28, 4, "Stamp", "", 0, "C", false, 0, "")
	}

	qrPNG, err := qrcode.Encode(verifyURL, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("prescriptions: generate qr code: %w", err)
	}
	const qrSize = 28.0
	qrX := pdfRight - qrSize
	pdf.RegisterImageOptionsReader("qr-verify", fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(qrPNG))
	pdf.ImageOptions("qr-verify", qrX, sigY, qrSize, qrSize, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	pdf.SetXY(qrX-4, sigY+qrSize+1)
	pdf.SetFont("Arial", "", 7)
	pdf.SetTextColor(90, 90, 90)
	pdf.CellFormat(qrSize+8, 4, "Scan to verify", "", 1, "C", false, 0, "")

	// --- footer ------------------------------------------------------------
	pdf.SetY(-16)
	pdf.SetDrawColor(brandMintR, brandMintG, brandMintB)
	pdf.SetLineWidth(0.6)
	pdf.Line(pdfLeft, pdf.GetY(), pdfRight, pdf.GetY())
	pdf.Ln(1.5)
	pdf.SetFont("Arial", "", 7)
	pdf.SetTextColor(110, 110, 110)
	pdf.CellFormat(contentW, 3.5, "Issued by VersaLife Telemedicine. A pharmacist can confirm this prescription by scanning the code.", "", 1, "C", false, 0, "")
	pdf.CellFormat(contentW, 3.5, "Prescription "+p.ID.String(), "", 1, "C", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("prescriptions: render pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// pdfDoctorName title-cases a name that was stored in all lowercase
// ("amara perera") so the pad reads "Amara Perera". A name that already
// has capitals is left alone.
func pdfDoctorName(name string) string {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	for _, prefix := range []string{"dr. ", "dr "} {
		if strings.HasPrefix(lower, prefix) {
			name = strings.TrimSpace(name[len(prefix):])
			lower = strings.ToLower(name)
			break
		}
	}
	if name != lower {
		return pdfSafe(name)
	}
	parts := strings.Fields(name)
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return pdfSafe(strings.Join(parts, " "))
}

// pdfSafe maps the string onto the cp1252 repertoire fpdf's core fonts
// actually draw. A UTF-8 en dash otherwise prints as "â€“".
func pdfSafe(s string) string {
	s = strings.NewReplacer(
		"\u2013", "-",
		"\u2014", "-",
		"\u2018", "'",
		"\u2019", "'",
		"\u201c", `"`,
		"\u201d", `"`,
		"\u00a0", " ",
	).Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r >= 32 && r <= 255:
			b.WriteRune(r)
		}
	}
	return b.String()
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
