package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/middleware"
)

func apiServer(t *testing.T, h *harness) http.Handler {
	t.Helper()
	renderer, err := NewInvoiceRenderer(InvoiceBranding{CompanyName: "Telemed", Timezone: "Asia/Colombo"})
	require.NoError(t, err)

	runner := NewPayoutRunner(h.store, h.svc.providers, StaticDestinations{}, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock,
	}).WithClock(func() time.Time { return h.now })

	return NewHandler(h.svc, runner, renderer, zerolog.Nop()).Routes()
}

// as issues a request carrying an authenticated principal, the way RequireAuth
// would have after verifying a signature.
func as(t *testing.T, srv http.Handler, p middleware.Principal, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(middleware.WithPrincipal(req.Context(), p))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func patient(id uuid.UUID) middleware.Principal {
	return middleware.Principal{UserID: id, Roles: []middleware.Role{middleware.RolePatient}}
}

func doctor(userID, doctorID uuid.UUID) middleware.Principal {
	return middleware.Principal{UserID: userID, DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
}

func finance() middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleFinance}}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

// TestPaymentIsReadableOnlyByItsParties.
func TestPaymentIsReadableOnlyByItsParties(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	srv := apiServer(t, h)
	path := "/" + p.ID.String()

	assert.Equal(t, http.StatusOK, as(t, srv, patient(p.PatientID), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusOK, as(t, srv, doctor(uuid.New(), p.DoctorID), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusOK, as(t, srv, finance(), http.MethodGet, path, nil).Code)

	rec := as(t, srv, patient(uuid.New()), http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "FORBIDDEN", decode(t, rec)["code"])
}

// TestLedgerIsFinanceOnly: the double-entry view is finance's, not the
// patient's receipt.
func TestLedgerIsFinanceOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)
	path := "/" + p.ID.String() + "/ledger"

	assert.Equal(t, http.StatusForbidden, as(t, srv, patient(p.PatientID), http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusForbidden, as(t, srv, doctor(uuid.New(), p.DoctorID), http.MethodGet, path, nil).Code)

	rec := as(t, srv, finance(), http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	entries, ok := decode(t, rec)["data"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 4)
}

// A patient asserting cancelled_by=doctor or naming their own amount_cents was
// the first shape of this bug: it would have upgraded their own refund. Both
// claims are still ignored -- but the request is now refused outright, because
// the deeper problem was never the number.
//
// See TestPatientCannotRefundThemselvesAtAll.
func TestPatientCannotClaimTheDoctorCancelled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"cancelled_by": "doctor", // the lie
		"start_at":     h.now.Add(30 * time.Minute).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "REFUND_NOT_ALLOWED", decode(t, rec)["code"])
	assert.Zero(t, h.refundCount(t, p.ID), "no refund row may exist")
}

// TestPatientCannotNameTheirOwnRefundAmount.
func TestPatientCannotNameTheirOwnRefundAmount(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"amount_cents": 500_000,
		"start_at":     h.now.Add(10 * time.Minute).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Zero(t, h.refundCount(t, p.ID))
}

// TestFinanceMayAssertTheCancellationFacts.
func TestFinanceMayAssertTheCancellationFacts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, finance(), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"cancelled_by": "doctor",
		"start_at":     h.now.Add(10 * time.Minute).Format(time.RFC3339),
		"cancelled_at": h.now.Format(time.RFC3339),
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	data := decode(t, rec)["data"].(map[string]any)
	assert.Equal(t, float64(100), data["percent"])
	assert.Equal(t, "doctor_cancelled", data["reason"])
}

// TestRefundOnAnUnsettledPaymentIs422.
func TestRefundOnAnUnsettledPaymentIs422(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, finance(), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"cancelled_by": "patient",
		"start_at":     h.now.Add(6 * time.Hour).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "REFUND_NOT_ALLOWED", decode(t, rec)["code"])
}

// TestNoShowRefundIs422WithTheRightCode: the policy returns nothing, and the
// client must be able to tell that apart from a server error.
func TestNoShowRefundIs422(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, finance(), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"cancelled_by": "patient",
		"no_show":      true,
		"start_at":     h.now.Add(-time.Hour).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "REFUND_NOT_ALLOWED", decode(t, rec)["code"])
}

// TestIntentBeforeAppointmentCreatedIs409.
func TestIntentBeforeAppointmentCreatedIs409(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := apiServer(t, h)

	rec := as(t, srv, patient(uuid.New()), http.MethodPost, "/intent", map[string]any{
		"appointment_id": uuid.NewString(),
	})
	require.Equal(t, http.StatusConflict, rec.Code)
	body := decode(t, rec)
	assert.Equal(t, "PAYMENT_NOT_READY", body["code"])
	assert.Contains(t, body["message"], "retry")
}

// TestIntentRejectsUnknownFieldsAndBadProviders.
func TestIntentRejectsBadRequests(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 500_000)
	srv := apiServer(t, h)
	caller := patient(p.PatientID)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing appointment_id", map[string]any{}, http.StatusUnprocessableEntity},
		{"appointment_id not a uuid", map[string]any{"appointment_id": "not-a-uuid"}, http.StatusUnprocessableEntity},
		{"unknown provider", map[string]any{"appointment_id": p.AppointmentID.String(), "provider": "paypal"}, http.StatusUnprocessableEntity},
		{"unconfigured rail", map[string]any{"appointment_id": p.AppointmentID.String(), "provider": "payhere"}, http.StatusNotImplemented},
		{"unknown field", map[string]any{"appointment_id": p.AppointmentID.String(), "amount_cents": 1}, http.StatusBadRequest},
		{"non sri lankan phone", map[string]any{"appointment_id": p.AppointmentID.String(), "phone": "+14155550123"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(t, srv, caller, http.MethodPost, "/intent", tc.body)
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// TestListPaymentsIsScopedToTheCaller.
func TestListPaymentsIsScopedToTheCaller(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	mine := h.pendingPayment(t, 100_000)
	h.pendingPayment(t, 200_000) // someone else's
	srv := apiServer(t, h)

	rec := as(t, srv, patient(mine.PatientID), http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	body := decode(t, rec)
	items := body["data"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, mine.ID.String(), items[0].(map[string]any)["id"])

	meta := body["meta"].(map[string]any)
	assert.Equal(t, float64(1), meta["total"])
	assert.Equal(t, float64(1), meta["page"])
}

// TestInvoiceIsAPDFAndUncacheable.
func TestInvoiceIsAPDFAndUncacheable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, patient(p.PatientID), http.MethodGet, "/"+p.ID.String()+"/invoice", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/pdf", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "invoice-"+p.ID.String()+".pdf")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	assert.Equal(t, "%PDF", rec.Body.String()[:4])
}

// TestFinancialResponsesAreNoStore: no proxy, browser or CDN keeps a copy of a
// payment.
func TestFinancialResponsesAreNoStore(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 100_000)
	srv := apiServer(t, h)

	rec := as(t, srv, patient(p.PatientID), http.MethodGet, "/"+p.ID.String(), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "private")
}

// TestPayoutRunIsSuperAdminOnly.
func TestPayoutRunIsSuperAdminOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	renderer, err := NewInvoiceRenderer(InvoiceBranding{CompanyName: "Telemed", Timezone: "Asia/Colombo"})
	require.NoError(t, err)
	runner := NewPayoutRunner(h.store, h.svc.providers, StaticDestinations{}, zerolog.Nop(), PayoutConfig{
		HoldPeriod: 24 * time.Hour, Provider: ProviderMock,
	}).WithClock(func() time.Time { return h.now })
	srv := NewHandler(h.svc, runner, renderer, zerolog.Nop()).PayoutRoutes()

	for _, p := range []middleware.Principal{
		patient(uuid.New()),
		doctor(uuid.New(), uuid.New()),
		finance(),
		{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleOps}},
	} {
		rec := as(t, srv, p, http.MethodPost, "/run", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "roles %v must not be able to run payouts", p.Roles)
	}

	superAdmin := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleSuperAdmin}}
	rec := as(t, srv, superAdmin, http.MethodPost, "/run", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "\"paid\"")
}

// TestPayoutListIsScopedToTheDoctor: a doctor sees their own settlements and
// nobody else's; a caller who is neither doctor nor finance sees an empty list
// rather than a 403, which would leak that payouts exist at all.
func TestPayoutListIsScoped(t *testing.T) {
	t.Parallel()

	h := newPayoutHarness(t, 2, 1, 500_000)
	_, err := h.runner.Run(t.Context())
	require.NoError(t, err)

	renderer, err := NewInvoiceRenderer(InvoiceBranding{CompanyName: "Telemed", Timezone: "Asia/Colombo"})
	require.NoError(t, err)
	srv := NewHandler(h.svc, h.runner, renderer, zerolog.Nop()).PayoutRoutes()

	rec := as(t, srv, doctor(uuid.New(), h.doctors[0]), http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, decode(t, rec)["data"].([]any), 1)

	rec = as(t, srv, finance(), http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, decode(t, rec)["data"].([]any), 2)

	rec = as(t, srv, patient(uuid.New()), http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, decode(t, rec)["data"].([]any))
}

// TestConfirmPINRejectsAWrongState.
func TestConfirmPINRejectsAWrongState(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.pendingPayment(t, 100_000) // requires_action, not requires_pin
	srv := apiServer(t, h)

	rec := as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/confirm-pin",
		map[string]any{"pin": "123456"})
	assert.Equal(t, http.StatusConflict, rec.Code,
		"confirming a PIN against a payment with no outstanding challenge is a state "+
			"conflict the client can see and explain, not an internal error")
	assert.Equal(t, "PIN_NOT_OUTSTANDING", decode(t, rec)["code"])
}

// TestUnknownPaymentIs404WithAValidUUID and 400 with a malformed one.
func TestPathIDHandling(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	srv := apiServer(t, h)
	caller := finance()

	rec := as(t, srv, caller, http.MethodGet, "/"+uuid.NewString(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = as(t, srv, caller, http.MethodGet, "/not-a-uuid", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "BAD_REQUEST", decode(t, rec)["code"])
}

// TestPatientCannotBuyThemselvesAFullRefundWithStartAt is the regression test
// for a self-service money tap.
//
// The refund percentage for the patient branch is decided purely by
// notice = start_at - cancelled_at (policy.go:105): at least
// LateCancellationWindow of notice is 100%, less is 50%. `start_at` was read
// from the request body for EVERY caller, not just ops -- the assignment sat
// one line below the closing brace of the `if ops` block. So a patient POSTed
// a start time far enough in the future to clear the window and was refunded
// 100% of their own captured payment.
//
// Nothing on this path checks that the appointment was cancelled, or that the
// consultation did not happen, so the sequence was: attend the consultation,
// then refund it in full.
//
// The two existing tests either side of this one both send `start_at`, but
// both send a value INSIDE the two-hour window, so both would have passed with
// the hole wide open. The distance of the timestamp is the whole bug.
func TestPatientCannotBuyThemselvesAFullRefundWithStartAt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		startAt any
	}{
		{"a decade in the future", "2035-01-01T00:00:00Z"},
		{"just past the late-cancellation window", nil}, // filled in below
		{"omitted entirely", nil},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			p := h.captured(t, 500_000)
			srv := apiServer(t, h)

			body := map[string]any{}
			switch i {
			case 0:
				body["start_at"] = tc.startAt
			case 1:
				body["start_at"] = h.now.Add(LateCancellationWindow + time.Hour).Format(time.RFC3339)
			case 2:
				// no start_at at all: the honest request shape
			}

			rec := as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/refund", body)
			require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "REFUND_NOT_ALLOWED", decode(t, rec)["code"])
			assert.Zero(t, h.refundCount(t, p.ID),
				"a patient moved money out of a captured payment by POSTing to /refund")
		})
	}
}

// TestOpsMayStillReplayACancellationWithStartAt guards against the wrong fix.
// Deleting start_at outright would break the case it exists for: finance
// replaying a cancellation that happened outside the normal event flow, where
// the real start time is a fact ops knows and the service does not.
func TestOpsMayStillReplayACancellationWithStartAt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, finance(), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"start_at": h.now.Add(LateCancellationWindow + time.Hour).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	data := decode(t, rec)["data"].(map[string]any)
	assert.Equal(t, float64(100), data["percent"],
		"finance replaying an early cancellation must still reach the 100%% branch")
}

// TestPatientCannotRefundThemselvesAtAll is the finding the percentage guard
// did not reach.
//
// The earlier fix stopped a patient CHOOSING the number. It did not establish
// that a refund was due at all, and the honest request shape -- an empty body
// -- still worked: with start_at zeroed, DecideRefund sees a notice period of
// roughly minus two thousand years, lands on the patient_cancelled_late
// branch, and hands back 50% of a captured payment. Nothing on the path checks
// that the appointment was ever cancelled, so the sequence was: attend the
// consultation, then POST an empty body and keep half the fee.
//
// And 50% was not the ceiling. The derived idempotency key is
// "refund:<payment>:<reason>", and refundRequest validates `reason` as oneof
// six constants -- so a second POST under a different reason produced a
// different key, passed Status.Settled() (which includes partially_refunded),
// and was handed the remaining 50%. Two requests, a full refund, and a
// `refunds` row stamped "doctor_cancelled" in the finance ledger.
//
// Refunds follow from cancelling the appointment: scheduling owns the policy,
// decides the percentage, and carries it on appointment.cancelled, which
// OnAppointmentCancelled applies as the platform rather than as a user.
func TestPatientCannotRefundThemselvesAtAll(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		name string
		body map[string]any
	}{
		{"an empty body, the honest request shape", map[string]any{}},
		{"naming a reason", map[string]any{"reason": "doctor_cancelled"}},
		{"naming a different reason", map[string]any{"reason": "duplicate"}},
		{"claiming a no-show", map[string]any{"no_show": true}},
		{"claiming a cancellation instant", map[string]any{"cancelled_at": "2020-01-01T00:00:00Z"}},
	}

	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			p := h.captured(t, 500_000)
			srv := apiServer(t, h)

			rec := as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/refund", tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "REFUND_NOT_ALLOWED", decode(t, rec)["code"])
			assert.Zero(t, h.refundCount(t, p.ID), "money moved out of a captured payment")

			after, err := h.store.GetPayment(context.Background(), p.ID)
			require.NoError(t, err)
			assert.Zero(t, after.RefundedCents)
			assert.Equal(t, StatusSucceeded, after.Status)
		})
	}
}

// TestPatientCannotDoubleRefundByVaryingTheReason is the escalation half,
// asserted directly rather than inferred: two POSTs under different reasons
// must not add up to more than one refund. It fails against a build where
// Refund still permits a non-ops caller but zeroes only the percentage inputs.
func TestPatientCannotDoubleRefundByVaryingTheReason(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	for _, reason := range []string{"patient_cancelled_late", "doctor_cancelled", "duplicate", "no_show", "admin_override"} {
		as(t, srv, patient(p.PatientID), http.MethodPost, "/"+p.ID.String()+"/refund",
			map[string]any{"reason": reason})
	}

	after, err := h.store.GetPayment(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Zero(t, after.RefundedCents,
		"a patient recovered %d cents of a %d cent payment by varying `reason` across the derived idempotency key",
		after.RefundedCents, p.AmountCents)
	assert.Zero(t, h.refundCount(t, p.ID))
}

// TestOpsAndTheCancellationEventCanStillRefund guards against the wrong fix:
// the legitimate paths must both keep working.
func TestOpsAndTheCancellationEventCanStillRefund(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	p := h.captured(t, 500_000)
	srv := apiServer(t, h)

	rec := as(t, srv, finance(), http.MethodPost, "/"+p.ID.String()+"/refund", map[string]any{
		"cancelled_by": "doctor",
		"start_at":     h.now.Add(LateCancellationWindow + time.Hour).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	data := decode(t, rec)["data"].(map[string]any)
	assert.Equal(t, float64(100), data["percent"], "finance must still be able to refund in full")
}
