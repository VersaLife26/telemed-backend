package payment

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRenderer(t *testing.T) *InvoiceRenderer {
	t.Helper()
	r, err := NewInvoiceRenderer(InvoiceBranding{
		CompanyName: "Telemed Lanka (Pvt) Ltd",
		AddressLine: "Colombo 03, Sri Lanka",
		TaxID:       "1234567890",
		Email:       "billing@telemed.lk",
		Timezone:    "Asia/Colombo",
	})
	require.NoError(t, err)
	return r
}

func sampleInvoice() Invoice {
	at := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	ruleID := uuid.New()
	return Invoice{
		Payment: Payment{
			ID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
			AmountCents: 500_000, Currency: CurrencyLKR, Provider: ProviderStripe,
			Status: StatusSucceeded, CommissionCents: 100_000, ProviderFeeCents: 15_000,
			DoctorPayoutCents: 385_000, CommissionRuleID: &ruleID,
			CommissionRuleKey: "specialty:gp", CommissionRuleVer: 1,
			SucceededAt: &at, CreatedAt: at, UpdatedAt: at,
		},
		Rule: CommissionRule{
			ID: ruleID, RuleKey: "specialty:gp", Version: 1, Scope: ScopeSpecialty,
			MatchValue: "GP", RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		},
	}
}

func TestInvoiceRendersAPDF(t *testing.T) {
	t.Parallel()

	pdf, err := testRenderer(t).Render(sampleInvoice())
	require.NoError(t, err)
	require.NotEmpty(t, pdf)
	assert.Equal(t, "%PDF", string(pdf[:4]), "the response must actually be a PDF")
	assert.Greater(t, len(pdf), 1000)
}

// TestInvoiceHidesTheSplitFromPatients: a receipt shows what the patient paid.
// What the platform kept and what the doctor earned is between us and them.
func TestInvoiceHidesTheSplitFromPatients(t *testing.T) {
	t.Parallel()

	in := sampleInvoice()

	patient, err := testRenderer(t).Render(in)
	require.NoError(t, err)

	in.ShowSplit = true
	doctor, err := testRenderer(t).Render(in)
	require.NoError(t, err)

	assert.Greater(t, len(doctor), len(patient),
		"the settlement breakdown must only appear on the doctor and finance copy")
}

func TestInvoiceRendersARefundedPayment(t *testing.T) {
	t.Parallel()

	in := sampleInvoice()
	in.Payment.Status = StatusPartiallyRefunded
	in.Payment.RefundedCents = 250_000
	in.Payment.RefundedCommissionCents = 50_000
	in.Payment.RefundedPayoutCents = 200_000
	in.ShowSplit = true
	in.Refunds = []Refund{{
		ID: uuid.New(), PaymentID: in.Payment.ID, AmountCents: 250_000, Currency: CurrencyLKR,
		Reason: ReasonPatientCancelledLate, Percent: 50, Status: RefundSucceeded,
	}}

	pdf, err := testRenderer(t).Render(in)
	require.NoError(t, err)
	assert.Equal(t, "%PDF", string(pdf[:4]))
}

func TestInvoiceRendersWithoutARule(t *testing.T) {
	t.Parallel()

	in := sampleInvoice()
	in.Rule = CommissionRule{}
	in.ShowSplit = true

	pdf, err := testRenderer(t).Render(in)
	require.NoError(t, err, "a missing rule must not stop an invoice being produced")
	assert.Equal(t, "%PDF", string(pdf[:4]))
}

func TestInvoiceRejectsABadTimezone(t *testing.T) {
	t.Parallel()

	_, err := NewInvoiceRenderer(InvoiceBranding{Timezone: "Mars/Olympus_Mons"})
	assert.Error(t, err)
}

func TestShortRef(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "3F1A-6B7C-0000", shortRef("3f1a6b7c-0000-4000-8000-000000000001"))
	assert.Equal(t, "SHORT", shortRef("short"))
}

func TestProviderLabelIsPatientFacing(t *testing.T) {
	t.Parallel()

	// The invoice says how they paid, not which vendor we route through.
	assert.Equal(t, "Card", providerLabel(ProviderStripe))
	assert.Equal(t, "Mobile bill", providerLabel(ProviderDialog))
	assert.Equal(t, "PayHere", providerLabel(ProviderPayHere))
}
