package payment

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// Invoice rendering.
//
// The invoice is a legal document, so two properties matter more than looking
// nice:
//
//  1. It is reproducible. Every number on it comes from the payment row and
//     from the exact commission_rules version that priced it, both of which are
//     immutable. Regenerating this PDF in 2033 produces the same figures as
//     today, even if the commission rate has changed five times since.
//  2. It never contains clinical information. The line item is "Telemedicine
//     consultation" and an appointment reference. Not a symptom, not a
//     diagnosis, not a specialty note. An invoice is routinely forwarded to an
//     employer or an insurer, and it must be safe to forward.

// InvoiceBranding is the issuing entity's details. They come from
// configuration, not from a hardcoded string, because the platform is expected
// to be white-labelled for hospital clients.
type InvoiceBranding struct {
	CompanyName string
	AddressLine string
	TaxID       string // TIN/VAT registration
	Email       string
	Phone       string
	// Timezone renders dates in the reader's local time. An invoice dated in
	// UTC for a Colombo patient is off by a day for anything after 18:30.
	Timezone string
}

// Invoice is everything the renderer needs.
type Invoice struct {
	Payment Payment
	Refunds []Refund
	Rule    CommissionRule
	// ShowSplit reveals the commission and payout breakdown. Patients see only
	// what they paid.
	ShowSplit bool
}

// InvoiceRenderer produces invoice PDFs.
type InvoiceRenderer struct {
	brand InvoiceBranding
	loc   *time.Location
}

// NewInvoiceRenderer builds the renderer, resolving the timezone once.
func NewInvoiceRenderer(brand InvoiceBranding) (*InvoiceRenderer, error) {
	tz := brand.Timezone
	if tz == "" {
		tz = "Asia/Colombo"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("payment: invoice timezone %q: %w", tz, err)
	}
	if brand.CompanyName == "" {
		brand.CompanyName = "Telemed"
	}
	return &InvoiceRenderer{brand: brand, loc: loc}, nil
}

const (
	pageMargin  = 15.0
	contentWide = 180.0
)

// Render produces the PDF bytes.
func (r *InvoiceRenderer) Render(in Invoice) ([]byte, error) {
	p := in.Payment

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(pageMargin, pageMargin, pageMargin)
	pdf.SetTitle("Invoice "+shortRef(p.ID.String()), true)
	pdf.SetCreator(r.brand.CompanyName, true)
	pdf.AddPage()

	r.header(pdf, p)
	r.parties(pdf, p)
	r.lineItems(pdf, in)
	r.totals(pdf, in)
	if in.ShowSplit {
		r.settlement(pdf, in)
	}
	r.footer(pdf, p)

	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("payment: build invoice pdf: %w", err)
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("payment: write invoice pdf: %w", err)
	}
	return buf.Bytes(), nil
}

func (r *InvoiceRenderer) header(pdf *fpdf.Fpdf, p Payment) {
	pdf.SetFont("Helvetica", "B", 18)
	pdf.SetTextColor(20, 30, 45)
	pdf.CellFormat(contentWide/2, 10, r.brand.CompanyName, "", 0, "L", false, 0, "")

	pdf.SetFont("Helvetica", "B", 18)
	title := "INVOICE"
	if p.Status == StatusRefunded {
		title = "CREDIT NOTE"
	}
	pdf.CellFormat(contentWide/2, 10, title, "", 1, "R", false, 0, "")

	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(90, 100, 110)
	left := []string{r.brand.AddressLine}
	if r.brand.TaxID != "" {
		left = append(left, "TIN: "+r.brand.TaxID)
	}
	if r.brand.Email != "" {
		left = append(left, r.brand.Email)
	}
	if r.brand.Phone != "" {
		left = append(left, r.brand.Phone)
	}

	issued := p.CreatedAt
	if p.SucceededAt != nil {
		issued = *p.SucceededAt
	}
	right := []string{
		"Invoice no.  " + shortRef(p.ID.String()),
		"Issued       " + issued.In(r.loc).Format("02 Jan 2006 15:04 MST"),
		"Status       " + strings.ToUpper(string(p.Status)),
	}

	for i := 0; i < maxInt(len(left), len(right)); i++ {
		l, rt := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			rt = right[i]
		}
		pdf.CellFormat(contentWide/2, 5, l, "", 0, "L", false, 0, "")
		pdf.CellFormat(contentWide/2, 5, rt, "", 1, "R", false, 0, "")
	}

	pdf.Ln(4)
	pdf.SetDrawColor(210, 216, 222)
	y := pdf.GetY()
	pdf.Line(pageMargin, y, pageMargin+contentWide, y)
	pdf.Ln(6)
}

func (r *InvoiceRenderer) parties(pdf *fpdf.Fpdf, p Payment) {
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(20, 30, 45)
	pdf.CellFormat(contentWide/2, 5, "BILLED TO", "", 0, "L", false, 0, "")
	pdf.CellFormat(contentWide/2, 5, "CONSULTATION", "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(60, 70, 80)
	// Identifiers only. The invoice carries no name, no phone number and no NIC:
	// this document leaves our control the moment it is downloaded.
	rows := [][2]string{
		{"Patient ref.  " + shortRef(p.PatientID.String()), "Appointment  " + shortRef(p.AppointmentID.String())},
		{"", "Doctor ref.  " + shortRef(p.DoctorID.String())},
		{"", "Paid via     " + providerLabel(p.Provider)},
	}
	for _, row := range rows {
		pdf.CellFormat(contentWide/2, 5, row[0], "", 0, "L", false, 0, "")
		pdf.CellFormat(contentWide/2, 5, row[1], "", 1, "L", false, 0, "")
	}
	pdf.Ln(6)
}

func (r *InvoiceRenderer) lineItems(pdf *fpdf.Fpdf, in Invoice) {
	p := in.Payment

	pdf.SetFillColor(240, 243, 246)
	pdf.SetTextColor(20, 30, 45)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(120, 8, "  Description", "", 0, "L", true, 0, "")
	pdf.CellFormat(60, 8, "Amount ("+p.Currency+")  ", "", 1, "R", true, 0, "")

	pdf.SetFont("Helvetica", "", 10)
	pdf.SetTextColor(40, 50, 60)
	// Deliberately generic. No specialty, no symptom, no diagnosis.
	pdf.CellFormat(120, 8, "  Telemedicine consultation", "", 0, "L", false, 0, "")
	pdf.CellFormat(60, 8, FormatLKR(p.AmountCents)+"  ", "", 1, "R", false, 0, "")

	// Ranged by index rather than by value: a Refund is a few hundred bytes and
	// copying it per iteration buys nothing.
	for i := range in.Refunds {
		rf := &in.Refunds[i]
		if rf.Status != RefundSucceeded {
			continue
		}
		pdf.SetTextColor(150, 60, 60)
		label := fmt.Sprintf("  Refund (%d%%) - %s", rf.Percent, humanReason(rf.Reason))
		pdf.CellFormat(120, 8, label, "", 0, "L", false, 0, "")
		pdf.CellFormat(60, 8, "-"+FormatLKR(rf.AmountCents)+"  ", "", 1, "R", false, 0, "")
	}
	pdf.SetTextColor(40, 50, 60)
	pdf.Ln(2)
}

func (r *InvoiceRenderer) totals(pdf *fpdf.Fpdf, in Invoice) {
	p := in.Payment
	net := p.AmountCents - p.RefundedCents

	pdf.SetDrawColor(210, 216, 222)
	y := pdf.GetY()
	pdf.Line(pageMargin+100, y, pageMargin+contentWide, y)
	pdf.Ln(2)

	pdf.SetFont("Helvetica", "", 10)
	pdf.CellFormat(120, 6, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(60, 6, "Charged   "+FormatLKR(p.AmountCents)+"  ", "", 1, "R", false, 0, "")

	if p.RefundedCents > 0 {
		pdf.CellFormat(120, 6, "", "", 0, "L", false, 0, "")
		pdf.CellFormat(60, 6, "Refunded   -"+FormatLKR(p.RefundedCents)+"  ", "", 1, "R", false, 0, "")
	}

	pdf.SetFont("Helvetica", "B", 12)
	pdf.SetTextColor(20, 30, 45)
	pdf.CellFormat(120, 9, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(60, 9, "NET PAID   "+p.Currency+" "+FormatLKR(net)+"  ", "", 1, "R", false, 0, "")
	pdf.Ln(4)
}

// settlement prints the commission breakdown, for the doctor and for finance.
// It states the exact rule version used, which is what makes a payout dispute
// answerable from the document itself.
func (r *InvoiceRenderer) settlement(pdf *fpdf.Fpdf, in Invoice) {
	p := in.Payment

	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(20, 30, 45)
	pdf.CellFormat(contentWide, 7, "SETTLEMENT BREAKDOWN", "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(60, 70, 80)

	rateLabel := "-"
	roundLabel := string(DefaultRounding)
	if in.Rule.RuleKey != "" {
		rateLabel = fmt.Sprintf("%s (rule %s v%d)", BpsString(in.Rule.RateBps), in.Rule.RuleKey, in.Rule.Version)
		if in.Rule.Rounding.Valid() {
			roundLabel = string(in.Rule.Rounding)
		}
	} else if p.CommissionRuleKey != "" {
		rateLabel = fmt.Sprintf("rule %s v%d", p.CommissionRuleKey, p.CommissionRuleVer)
	}

	rows := [][2]string{
		{"Gross consultation fee", FormatLKR(p.AmountCents)},
		{"Platform commission  " + rateLabel, "-" + FormatLKR(p.CommissionCents)},
		{"Processing fee (" + providerLabel(p.Provider) + ")", "-" + FormatLKR(p.ProviderFeeCents)},
		{"Doctor payout", FormatLKR(p.DoctorPayoutCents)},
	}
	if p.RefundedCents > 0 {
		rows = append(rows,
			[2]string{"Commission reversed on refund", FormatLKR(p.RefundedCommissionCents)},
			[2]string{"Doctor payout reversed on refund", FormatLKR(p.RefundedPayoutCents)},
			[2]string{"Net doctor payout", FormatLKR(p.NetPayoutCents())},
		)
	}

	for _, row := range rows {
		pdf.CellFormat(120, 6, "  "+row[0], "", 0, "L", false, 0, "")
		pdf.CellFormat(60, 6, row[1]+"  ", "", 1, "R", false, 0, "")
	}

	pdf.Ln(2)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.SetTextColor(120, 130, 140)
	pdf.MultiCell(contentWide, 4, fmt.Sprintf(
		"Commission, processing fee and doctor payout sum exactly to the gross fee (%s = %s + %s + %s). "+
			"Amounts are computed in cents with %s rounding under the commission rule version cited above; "+
			"that rule version is immutable, so this breakdown is reproducible for the life of the record.",
		FormatLKR(p.AmountCents), FormatLKR(p.CommissionCents),
		FormatLKR(p.ProviderFeeCents), FormatLKR(p.DoctorPayoutCents), roundLabel),
		"", "L", false)
	pdf.Ln(3)
}

func (r *InvoiceRenderer) footer(pdf *fpdf.Fpdf, p Payment) {
	pdf.SetY(-30)
	pdf.SetDrawColor(210, 216, 222)
	pdf.Line(pageMargin, pdf.GetY(), pageMargin+contentWide, pdf.GetY())
	pdf.Ln(3)

	pdf.SetFont("Helvetica", "", 7)
	pdf.SetTextColor(130, 138, 148)
	pdf.MultiCell(contentWide, 3.5, fmt.Sprintf(
		"Payment reference %s. Generated %s. "+
			"This document contains no clinical information. "+
			"Queries: quote the payment reference above.",
		p.ID, time.Now().In(r.loc).Format("02 Jan 2006 15:04 MST")),
		"", "L", false)
}

func providerLabel(p ProviderName) string {
	switch p {
	case ProviderStripe:
		return "Card"
	case ProviderPayHere:
		return "PayHere"
	case ProviderDialog:
		return "Mobile bill"
	case ProviderMock:
		return "Test"
	default:
		return string(p)
	}
}

func humanReason(r RefundReason) string {
	switch r {
	case ReasonDoctorCancelled:
		return "cancelled by doctor"
	case ReasonPatientCancelledEarly:
		return "cancelled with notice"
	case ReasonPatientCancelledLate:
		return "cancelled late"
	case ReasonNoShow:
		return "no show"
	case ReasonAdminOverride:
		return "adjusted by support"
	case ReasonDuplicate:
		return "duplicate charge"
	default:
		return string(r)
	}
}

// shortRef renders a UUID as a human-quotable reference. Support staff read
// these aloud on the phone; 36 characters is not usable, 8 is.
func shortRef(id string) string {
	id = strings.ToUpper(strings.ReplaceAll(id, "-", ""))
	if len(id) <= 12 {
		return id
	}
	return id[:4] + "-" + id[4:8] + "-" + id[8:12]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
