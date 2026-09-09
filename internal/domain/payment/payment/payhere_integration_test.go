//go:build integration

// Integration tests that need a real provider implementation.
//
// They live in the external test package because internal/payment/provider/...
// imports internal/payment: an in-package test cannot import its own importer,
// so the only way to drive a genuine PayHere notify through the genuine service
// is from outside. The Postgres container is owned by TestMain in
// integration_test.go and reached through payment.IntegrationFreshSchema.
package payment_test

import (
	"context"
	"crypto/md5" //nolint:gosec // mandated by the PayHere 2.0 protocol
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/payment/payment"
	payhereprovider "telemed/internal/domain/payment/payment/provider/payhere"
	"telemed/internal/platform/events"
)

const (
	payhereMerchantID = "1221149"
	payhereSecret     = "MzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MA=="
)

// payhereNotifyForm builds the body PayHere actually posts to a notify URL.
//
// Every field below appears in a real PayHere 2.0 notify, including the ones
// this service never reads -- card metadata, the customer's own reference, the
// captured method. They are here because the point of the test is the whole
// body reaching webhook_events.payload, which is the dispute evidence: a test
// that posted only the six hashed fields would not have caught the defect it
// exists to pin, since the failure is in how the body is stored, not in how it
// is parsed.
func payhereNotifyForm(orderID, amount, currency, statusCode string) url.Values {
	sig := payhereprovider.NotifyHash(payhereMerchantID, orderID, amount, currency, statusCode,
		payhereSecretHash())
	return url.Values{
		"merchant_id":         {payhereMerchantID},
		"order_id":            {orderID},
		"payment_id":          {"320027112345"},
		"payhere_amount":      {amount},
		"payhere_currency":    {currency},
		"status_code":         {statusCode},
		"md5sig":              {sig},
		"custom_1":            {""},
		"custom_2":            {""},
		"method":              {"VISA"},
		"status_message":      {"Successfully completed the payment."},
		"card_holder_name":    {"P PERERA"},
		"card_no":             {"************1234"},
		"card_expiry":         {"1230"},
		"recurring":           {"0"},
		"payhere_created_at":  {"2026-08-20 14:32:11"},
		"item_recurring_flag": {"0"},
	}
}

// payhereSecretHash is MD5(merchant_secret) uppercased -- the inner hash both
// PayHere signatures are built on. It is computed here from the protocol
// definition rather than exported from the provider, because the inner MD5 of
// a merchant secret is not something that package should hand out, and because
// a test that re-derives the construction independently is a test that would
// notice the provider changing it.
func payhereSecretHash() string {
	sum := md5.Sum([]byte(payhereSecret)) //nolint:gosec // mandated by the PayHere 2.0 protocol
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// TestIntegrationPayHereNotifySettlesAndIsStored is the end-to-end proof for
// F12.
//
// PayHere's notify is form-encoded. WebhookEvent.Raw is written straight into
// webhook_events.payload, which is JSONB, and pgx v5 passes a []byte to a JSONB
// parameter without encoding it -- so Postgres parsed
// "merchant_id=...&order_id=..." as JSON, raised 22P02, rolled the whole
// transaction back, and the handler answered 500. Every PayHere notify. For
// ever. PayHere retries, the patient's money is taken, and the appointment is
// never confirmed.
//
// The only coverage this path had hardcoded '{}'::jsonb into the insert rather
// than driving ClaimWebhookEvent, which is precisely why the defect survived a
// green suite. This test drives a realistic notify body through the real HTTP
// route, the real provider, the real service and the real repository, and then
// reads the stored row back.
func TestIntegrationPayHereNotifySettlesAndIsStored(t *testing.T) {
	pool := payment.IntegrationFreshSchema(t)
	ctx := context.Background()
	repo := payment.NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	rule := payment.CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: payment.ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: payment.RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	_, err := repo.SeedRules(ctx, []payment.CommissionRule{rule})
	require.NoError(t, err)

	rail, err := payhereprovider.New(payhereprovider.Config{
		MerchantID:     payhereMerchantID,
		MerchantSecret: payhereSecret,
		BaseURL:        "https://sandbox.payhere.lk",
		NotifyURL:      "https://api.example.lk/webhooks/payhere",
		ReturnURL:      "https://app.example.lk/paid",
		CancelURL:      "https://app.example.lk/cancelled",
	})
	require.NoError(t, err)

	reg := payment.NewRegistry(payment.ProviderPayHere)
	reg.Register(rail)

	pricer := payment.NewPricer(repo, time.Minute, payment.CommissionRule{})
	require.NoError(t, pricer.Refresh(ctx))

	svc := payment.NewService(repo, pricer, reg, zerolog.Nop(), payment.Config{
		HoldPeriod: 24 * time.Hour, DefaultProvider: payment.ProviderPayHere,
		Currency: payment.CurrencyLKR,
	})

	appointmentID := uuid.New()
	require.NoError(t, svc.OnAppointmentCreated(ctx, events.AppointmentCreated{
		AppointmentID: appointmentID, PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 500_000, Currency: payment.CurrencyLKR, Specialty: "GP",
		StartAt: time.Now().Add(48 * time.Hour),
	}))

	created, err := repo.GetPaymentByAppointment(ctx, appointmentID)
	require.NoError(t, err)

	view, err := svc.CreateIntent(ctx, payment.CreateIntentInput{
		AppointmentID: appointmentID, CallerID: created.PatientID,
	})
	require.NoError(t, err)
	// PayHere's order id IS the payment id, which is what the notify carries
	// back and what resolvePayment matches on.
	require.Equal(t, created.ID.String(), view.Payment.ProviderIntentID)

	routes := payment.NewWebhookHandler(svc, zerolog.Nop()).Routes(nil)
	form := payhereNotifyForm(created.ID.String(),
		payhereprovider.FormatAmount(created.AmountCents), payment.CurrencyLKR, "2")

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/payhere",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"a valid PayHere notify must not 500; body: %s", rec.Body.String())

	// --- the payment settled
	settled, err := repo.GetPayment(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, payment.StatusSucceeded, settled.Status)
	assert.True(t, settled.SplitBalances())
	assert.Equal(t, int64(100_000), settled.CommissionCents)
	assert.Equal(t, int64(385_000), settled.DoctorPayoutCents)

	// --- and the delivery is on record, as JSONB, losslessly
	var provider, eventID, eventType, procErr string
	var payload []byte
	var storedPaymentID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT provider, event_id, event_type, payment_id, payload::text, processing_error
		FROM webhook_events`).Scan(&provider, &eventID, &eventType, &storedPaymentID, &payload, &procErr))

	assert.Equal(t, "payhere", provider)
	assert.Equal(t, created.ID.String()+":320027112345:2", eventID)
	assert.Equal(t, "payhere.notify.2", eventType)
	assert.Equal(t, created.ID, storedPaymentID)
	assert.Empty(t, procErr)

	require.True(t, json.Valid(payload), "webhook_events.payload is JSONB: %s", payload)
	var stored map[string][]string
	require.NoError(t, json.Unmarshal(payload, &stored))
	assert.Equal(t, []string{payhereMerchantID}, stored["merchant_id"])
	assert.Equal(t, []string{created.ID.String()}, stored["order_id"])
	assert.Equal(t, []string{"5000.00"}, stored["payhere_amount"])
	assert.Equal(t, []string{"LKR"}, stored["payhere_currency"])
	assert.Equal(t, []string{"2"}, stored["status_code"])
	assert.Equal(t, form["md5sig"], stored["md5sig"],
		"the signature is the proof of origin; it is what makes the row evidence")
	assert.Equal(t, []string{"VISA"}, stored["method"])
	assert.Equal(t, []string{"************1234"}, stored["card_no"],
		"PayHere sends a masked PAN and we keep it exactly as sent, in the one access-controlled place")

	// --- and a redelivery is a no-op, which needs the row to have been stored
	replay := httptest.NewRequestWithContext(ctx, http.MethodPost, "/payhere",
		strings.NewReader(form.Encode()))
	replay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	routes.ServeHTTP(rec2, replay)
	require.Equal(t, http.StatusOK, rec2.Code)

	var rows, ledgerLegs int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_events`).Scan(&rows))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_entries WHERE payment_id = $1`, created.ID).Scan(&ledgerLegs))
	assert.Equal(t, 1, rows, "two deliveries, one stored event")
	assert.Equal(t, 4, ledgerLegs, "two deliveries, one balanced capture")
}

// TestIntegrationPayHereNotifyAmountMismatchIsRefused is F10 on the rail that
// motivated F11: a genuine, correctly signed PayHere notify for an amount that
// is not what the payment says. It must not settle, and the delivery must be on
// record with the discrepancy.
//
// The signature here is real. That is the point -- nothing about the crypto is
// wrong, PayHere is simply reporting a different figure than the one we
// charged, and before this the service settled at the database's amount and
// never looked.
func TestIntegrationPayHereNotifyAmountMismatchIsRefused(t *testing.T) {
	pool := payment.IntegrationFreshSchema(t)
	ctx := context.Background()
	repo := payment.NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	rule := payment.CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: payment.ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: payment.RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	_, err := repo.SeedRules(ctx, []payment.CommissionRule{rule})
	require.NoError(t, err)

	rail, err := payhereprovider.New(payhereprovider.Config{
		MerchantID: payhereMerchantID, MerchantSecret: payhereSecret,
		BaseURL: "https://sandbox.payhere.lk",
	})
	require.NoError(t, err)
	reg := payment.NewRegistry(payment.ProviderPayHere)
	reg.Register(rail)

	pricer := payment.NewPricer(repo, time.Minute, payment.CommissionRule{})
	require.NoError(t, pricer.Refresh(ctx))
	svc := payment.NewService(repo, pricer, reg, zerolog.Nop(), payment.Config{
		HoldPeriod: 24 * time.Hour, DefaultProvider: payment.ProviderPayHere,
		Currency: payment.CurrencyLKR,
	})

	appointmentID := uuid.New()
	require.NoError(t, svc.OnAppointmentCreated(ctx, events.AppointmentCreated{
		AppointmentID: appointmentID, PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 500_000, Currency: payment.CurrencyLKR, Specialty: "GP",
		StartAt: time.Now().Add(48 * time.Hour),
	}))
	created, err := repo.GetPaymentByAppointment(ctx, appointmentID)
	require.NoError(t, err)
	_, err = svc.CreateIntent(ctx, payment.CreateIntentInput{
		AppointmentID: appointmentID, CallerID: created.PatientID,
	})
	require.NoError(t, err)

	routes := payment.NewWebhookHandler(svc, zerolog.Nop()).Routes(nil)
	// LKR 1.00 against a LKR 5,000.00 consultation, correctly signed.
	form := payhereNotifyForm(created.ID.String(), "1.00", payment.CurrencyLKR, "2")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/payhere",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "WEBHOOK_AMOUNT_MISMATCH")

	unsettled, err := repo.GetPayment(ctx, created.ID)
	require.NoError(t, err)
	assert.NotEqual(t, payment.StatusSucceeded, unsettled.Status)

	var legs int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_entries WHERE payment_id = $1`, created.ID).Scan(&legs))
	assert.Zero(t, legs, "an append-only ledger must not record a disputed capture")

	// The evidence is committed, not rolled back with the refusal.
	var procErr string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT processing_error FROM webhook_events`).Scan(&procErr))
	assert.Contains(t, procErr, "does not match")
	assert.Contains(t, procErr, "100")
	assert.Contains(t, procErr, "500000")
}

// TestIntegrationPayHereReplayHasASecondLayer is the answer to "is the dedup
// index genuinely sufficient".
//
// PayHere's notify carries no timestamp and no nonce, so there is nothing in a
// captured body that expires. The field-grammar validation makes the MD5
// preimage injective, so an attacker cannot alter any field and keep the
// signature -- which means what they hold is a byte-identical copy of a
// genuine notify, and the only thing standing between that copy and a second
// settlement is UNIQUE (provider, event_id) on webhook_events.
//
// The existing settle test proves a redelivery is a no-op WITH the dedup row
// present. This one removes the dedup row first, which is exactly what a
// webhook_events retention job would do -- and there is currently no retention
// policy on that table, so the index's sufficiency is silently coupled to it
// staying that way for ever. If that coupling is the whole defence, then the
// day somebody adds a sensible 90-day prune, every notify older than 90 days
// becomes replayable.
//
// It is not the whole defence, and this is the proof: with the dedup row gone,
// the captured notify is accepted, verified, and then refused by the payment's
// own state -- Status.Settled() short-circuits before applyCapture, so no
// second ledger leg is written and no money moves twice.
func TestIntegrationPayHereReplayHasASecondLayer(t *testing.T) {
	pool := payment.IntegrationFreshSchema(t)
	ctx := context.Background()
	repo := payment.NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	rule := payment.CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: payment.ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: payment.RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	_, err := repo.SeedRules(ctx, []payment.CommissionRule{rule})
	require.NoError(t, err)

	rail, err := payhereprovider.New(payhereprovider.Config{
		MerchantID:     payhereMerchantID,
		MerchantSecret: payhereSecret,
		BaseURL:        "https://sandbox.payhere.lk",
		NotifyURL:      "https://api.example.lk/webhooks/payhere",
		ReturnURL:      "https://app.example.lk/paid",
		CancelURL:      "https://app.example.lk/cancelled",
	})
	require.NoError(t, err)

	reg := payment.NewRegistry(payment.ProviderPayHere)
	reg.Register(rail)
	pricer := payment.NewPricer(repo, time.Minute, payment.CommissionRule{})
	require.NoError(t, pricer.Refresh(ctx))
	svc := payment.NewService(repo, pricer, reg, zerolog.Nop(), payment.Config{
		HoldPeriod: 24 * time.Hour, DefaultProvider: payment.ProviderPayHere,
		Currency: payment.CurrencyLKR,
	})

	appointmentID := uuid.New()
	require.NoError(t, svc.OnAppointmentCreated(ctx, events.AppointmentCreated{
		AppointmentID: appointmentID, PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 500_000, Currency: payment.CurrencyLKR, Specialty: "GP",
		StartAt: time.Now().Add(48 * time.Hour),
	}))
	created, err := repo.GetPaymentByAppointment(ctx, appointmentID)
	require.NoError(t, err)
	_, err = svc.CreateIntent(ctx, payment.CreateIntentInput{
		AppointmentID: appointmentID, CallerID: created.PatientID,
	})
	require.NoError(t, err)

	routes := payment.NewWebhookHandler(svc, zerolog.Nop()).Routes(nil)
	form := payhereNotifyForm(created.ID.String(),
		payhereprovider.FormatAmount(created.AmountCents), payment.CurrencyLKR, "2")
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/payhere",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, post().Code)
	settled, err := repo.GetPayment(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, payment.StatusSucceeded, settled.Status)

	var legsAfterFirst int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_entries WHERE payment_id = $1`, created.ID).Scan(&legsAfterFirst))
	require.Equal(t, 4, legsAfterFirst)

	// Simulate the retention job that does not exist yet: the dedup row is
	// gone, so the captured notify is, as far as the index is concerned, new.
	tag, err := pool.Exec(ctx, `DELETE FROM webhook_events`)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())

	require.Equal(t, http.StatusOK, post().Code,
		"a replay past the dedup index must still be accepted and recorded, not 500")

	after, err := repo.GetPayment(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, payment.StatusSucceeded, after.Status)
	assert.Equal(t, settled.AmountCents, after.AmountCents)
	assert.Equal(t, settled.CommissionCents, after.CommissionCents)
	assert.Equal(t, settled.DoctorPayoutCents, after.DoctorPayoutCents)

	var legsAfterReplay int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_entries WHERE payment_id = $1`, created.ID).Scan(&legsAfterReplay))
	assert.Equal(t, legsAfterFirst, legsAfterReplay,
		"a captured PayHere notify replayed past the dedup index wrote a second set of ledger legs; "+
			"the append-only ledger cannot be corrected in place, so this is unrecoverable")

	// And the replay is on record, which is what an investigator needs.
	var rows int
	var procErr string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_events`).Scan(&rows))
	assert.Equal(t, 1, rows)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(processing_error, '') FROM webhook_events`).Scan(&procErr))
	assert.Contains(t, procErr, "already settled",
		"the second layer must say WHY it refused, or an investigator cannot tell a replay from a duplicate delivery")
}
