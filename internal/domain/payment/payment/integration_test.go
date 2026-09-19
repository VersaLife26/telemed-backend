//go:build integration

// Integration tests. Run with: make test-integration
//
// These stand up a real Postgres 17 through testcontainers and exercise the
// things a fake cannot honestly test: that the migrations apply and roll back,
// that the CHECK constraints actually refuse an unbalanced split, that the
// append-only trigger actually refuses an UPDATE, and that the payout claim is
// exactly-once under real concurrency rather than under a mutex.
//
// The unit tests prove the logic. These prove the database agrees.
package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/repopath"
)

var sharedPool *pgxpool.Pool

// TestMain owns the container lifetime.
//
// It is TestMain rather than a sync.Once inside a helper because
// testcontainers' cleanup is registered against the *testing.T that happened to
// win the race -- which means the container is torn down when that one test
// ends, and every later test in the package finds a closed connection.
func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_payment"),
		tcpostgres.WithUsername("telemed"),
		tcpostgres.WithPassword("telemed"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: start postgres: %v\n", err)
		os.Exit(1)
	}

	code := func() int {
		defer func() {
			if err := testcontainers.TerminateContainer(container); err != nil {
				fmt.Fprintf(os.Stderr, "integration: terminate postgres: %v\n", err)
			}
		}()

		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			fmt.Fprintf(os.Stderr, "integration: connection string: %v\n", err)
			return 1
		}
		// Migrate into svc_payment, not public: that is the shape production runs.
		dsn, err = database.EnsureSchema(ctx, dsn, "payment")
		if err != nil {
			fmt.Fprintf(os.Stderr, "integration: provision schema: %v\n", err)
			return 1
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "integration: open pool: %v\n", err)
			return 1
		}
		defer pool.Close()
		sharedPool = pool

		return m.Run()
	}()

	os.Exit(code)
}

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	require.NotNil(t, sharedPool, "postgres container is not running")
	return sharedPool
}

// IntegrationFreshSchema exposes the container-backed, freshly migrated pool to
// the EXTERNAL test package in this directory (payment_test).
//
// That package has to exist for one reason: internal/payment/provider/payhere
// imports internal/payment, so an in-package test can never import the real
// PayHere provider back. A test that wants to drive an actual PayHere notify
// through the actual service therefore lives in payment_test -- and TestMain,
// which owns the container, can only be declared once per directory, here.
func IntegrationFreshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return freshSchema(t)
}

// migrationFiles returns the .up.sql or .down.sql files in order.
func migrationFiles(t *testing.T, suffix string) []string {
	t.Helper()
	dir := repopath.Migrations(t, "payment")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	if suffix == ".down.sql" {
		// Down migrations run newest first.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	ctx := context.Background()
	for _, path := range migrationFiles(t, suffix) {
		sql, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(t, err, "applying %s", path)
	}
}

// freshSchema drops everything and reapplies, so each test starts clean.
func freshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := startPostgres(t)
	ctx := context.Background()
	// The DOMAIN schema, not public. Consolidation moved these tables into
	// svc_payment, and dropping public instead cleared nothing -- every test
	// then saw the previous one's rows, which showed up as balances that
	// accumulated across cases rather than as an obvious "reset failed".
	_, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS svc_payment CASCADE; CREATE SCHEMA svc_payment;`)
	require.NoError(t, err)
	applyMigrations(t, pool, ".up.sql")
	return pool
}

// TestIntegrationMigrationsRoundTrip proves every .down.sql actually reverses
// its .up.sql. An untested rollback is how a 3am incident becomes a 6am one.
func TestIntegrationMigrationsRoundTrip(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// The DOMAIN schema, not public. Consolidation moved these tables into
	// svc_payment, and dropping public instead cleared nothing -- every test
	// then saw the previous one's rows, which showed up as balances that
	// accumulated across cases rather than as an obvious "reset failed".
	_, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS svc_payment CASCADE; CREATE SCHEMA svc_payment;`)
	require.NoError(t, err)

	applyMigrations(t, pool, ".up.sql")

	tables := []string{"outbox_events", "commission_rules", "payouts", "payments",
		"webhook_events", "refunds", "ledger_entries",
		"promo_codes", "promo_redemptions", "pin_challenges",
		"payment_customers", "payment_methods"}
	for _, tbl := range tables {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).Scan(&exists))
		assert.True(t, exists, "table %s must exist after migrating up", tbl)
	}

	applyMigrations(t, pool, ".down.sql")
	for _, tbl := range tables {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).Scan(&exists))
		assert.False(t, exists, "table %s must be gone after migrating down", tbl)
	}

	// And up again, proving the down did not leave anything behind that would
	// collide.
	applyMigrations(t, pool, ".up.sql")

	// 000003 adds columns to an existing table rather than creating one, so
	// the table-existence check above cannot see it. A down migration that
	// dropped the tables but left gross_amount_cents behind would pass every
	// assertion so far and then fail the next up with "column already exists".
	for _, col := range []string{"gross_amount_cents", "discount_cents", "promo_code"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			                WHERE table_name = 'payments' AND column_name = $1)`, col).Scan(&exists))
		assert.True(t, exists, "payments.%s must exist after migrating up", col)
	}
}

// TestIntegrationLedgerIsAppendOnly proves the immutability is enforced by the
// database, not by convention. A convention is not a control.
func TestIntegrationLedgerIsAppendOnly(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	entryID := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (entry_id, entry_type, account, direction, amount_cents, currency)
		VALUES ($1, 'payment.captured', 'asset:provider_receivable', 'debit', 1000, 'LKR'),
		       ($1, 'payment.captured', 'revenue:commission', 'credit', 1000, 'LKR')`, entryID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE ledger_entries SET amount_cents = 1 WHERE entry_id = $1`, entryID)
	require.Error(t, err, "UPDATE on ledger_entries must be refused")
	assert.Contains(t, err.Error(), "append-only")

	_, err = pool.Exec(ctx, `DELETE FROM ledger_entries WHERE entry_id = $1`, entryID)
	require.Error(t, err, "DELETE on ledger_entries must be refused")
	assert.Contains(t, err.Error(), "append-only")

	_, err = pool.Exec(ctx, `TRUNCATE ledger_entries`)
	require.Error(t, err, "TRUNCATE on ledger_entries must be refused")
	assert.Contains(t, err.Error(), "append-only")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&n))
	assert.Equal(t, 2, n, "the rows must still be there")
}

// TestIntegrationWebhookUniqueIsScopedByProvider is the idempotency mechanism,
// verified against the real index.
func TestIntegrationWebhookUniqueIsScopedByProvider(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	insert := func(provider, eventID string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO webhook_events (id, provider, event_id, payload)
			VALUES ($1, $2, $3, '{}'::jsonb)`, uuid.New(), provider, eventID)
		return err
	}

	require.NoError(t, insert("stripe", "evt_1"))
	require.Error(t, insert("stripe", "evt_1"), "the same event on the same rail must be rejected")
	require.NoError(t, insert("payhere", "evt_1"),
		"the same id on a different rail is a different event and must be accepted")
	require.NoError(t, insert("dialog", "evt_1"))
}

// TestIntegrationPaymentSplitConstraint proves Postgres refuses a settled
// payment whose parts do not reconstitute the whole -- the last line of defence
// behind the Go arithmetic.
func TestIntegrationPaymentSplitConstraint(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	insert := func(status string, amount, commission, fee, payout int64) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
			                      gross_amount_cents,
			                      provider, status, commission_cents, provider_fee_cents,
			                      doctor_payout_cents, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,$5,'stripe',$6,$7,$8,$9,$10)`,
			uuid.New(), uuid.New(), uuid.New(), uuid.New(), amount, status,
			commission, fee, payout, uuid.NewString())
		return err
	}

	// Pending payments carry no split yet, which is fine.
	require.NoError(t, insert("pending", 100000, 0, 0, 0))

	// A settled payment must balance exactly.
	require.NoError(t, insert("succeeded", 100000, 20000, 3000, 77000))
	require.Error(t, insert("succeeded", 100000, 20000, 3000, 77001),
		"a settled payment whose parts overshoot the amount must be refused")
	require.Error(t, insert("succeeded", 100000, 20000, 3000, 76999),
		"a settled payment whose parts undershoot the amount must be refused")

	// Refund clawbacks must reconstitute the refund.
	_, err := pool.Exec(ctx, `
		INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
		                      gross_amount_cents,
		                      provider, status, commission_cents, provider_fee_cents,
		                      doctor_payout_cents, refunded_cents, refunded_commission_cents,
		                      refunded_payout_cents, idempotency_key)
		VALUES ($1,$2,$3,$4,100000,100000,'stripe','partially_refunded',20000,3000,77000,50000,10000,39999,$5)`,
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.NewString())
	require.Error(t, err, "a refund split that does not sum to the refund must be refused")
}

// TestIntegrationCommissionRuleVersioning proves a rule cannot be mutated into
// two simultaneously-open versions, which is what would make a historical
// invoice ambiguous.
func TestIntegrationCommissionRuleVersioning(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	insertRule := func(key string, version int, closed bool) error {
		var effectiveTo any
		if closed {
			effectiveTo = time.Now().Add(time.Hour)
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO commission_rules (id, rule_key, version, scope, rate_bps, effective_to)
			VALUES ($1,$2,$3,'default',$4,$5)`, uuid.New(), key, version, 2000+version, effectiveTo)
		return err
	}

	require.NoError(t, insertRule("default", 1, false))
	require.Error(t, insertRule("default", 2, false),
		"two open versions of one rule must be refused; supersede by closing the old one first")
	require.NoError(t, insertRule("specialty:gp", 1, false))

	// The correct sequence: close v1, then open v2.
	_, err := pool.Exec(ctx, `UPDATE commission_rules SET effective_to = NOW() WHERE rule_key = 'default' AND version = 1`)
	require.NoError(t, err)
	require.NoError(t, insertRule("default", 2, false))

	// And the same version number can never be reused.
	require.Error(t, insertRule("default", 2, true))
}

// TestIntegrationRepositoryRoundTrip drives the real pgx repository through a
// full payment lifecycle: insert, lock, version-guarded update, ledger, outbox.
func TestIntegrationRepositoryRoundTrip(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	rule := CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	n, err := repo.SeedRules(ctx, []CommissionRule{rule})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Seeding is idempotent.
	n, err = repo.SeedRules(ctx, []CommissionRule{rule})
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	loaded, err := repo.GetRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.Equal(t, 2000, loaded.RateBps)
	assert.Equal(t, RoundHalfUp, loaded.Rounding)

	split, err := ComputeSplit(500_000, loaded)
	require.NoError(t, err)

	p := Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
		Specialty: "GP", AmountCents: 500_000, GrossAmountCents: 500_000, Currency: CurrencyLKR,
		Provider: ProviderStripe, Status: StatusPending,
		IdempotencyKey: "appointment:" + uuid.NewString(),
	}
	require.NoError(t, repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.InsertPayment(ctx, &p)
	}))
	assert.Equal(t, 1, p.Version)

	// A second insert for the same appointment must be refused.
	dup := p
	dup.ID = uuid.New()
	dup.IdempotencyKey = uuid.NewString()
	err = repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.InsertPayment(ctx, &dup)
	})
	assert.ErrorIs(t, err, ErrDuplicate)

	// Settle it, with the ledger and the outbox in the same transaction.
	now := time.Now().UTC()
	require.NoError(t, repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		locked.Status = StatusSucceeded
		locked.SucceededAt = &now
		locked.ProviderIntentID = "pi_integration"
		locked.CommissionCents = split.CommissionCents
		locked.ProviderFeeCents = split.ProviderFeeCents
		locked.DoctorPayoutCents = split.PayoutCents
		locked.CommissionRuleID = &rule.ID
		locked.CommissionRuleKey = rule.RuleKey
		locked.CommissionRuleVer = rule.Version
		if err := tx.UpdatePayment(ctx, &locked); err != nil {
			return err
		}
		pid, did := locked.ID, locked.DoctorID
		if err := tx.WriteLedger(ctx, LedgerTransaction{
			EntryID: uuid.New(), EntryType: EntryPaymentCaptured,
			Legs: []LedgerEntry{
				{Account: AccountProviderReceivable, Direction: Debit, AmountCents: locked.AmountCents,
					Currency: locked.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: now},
				{Account: AccountCommissionRevenue, Direction: Credit, AmountCents: locked.CommissionCents,
					Currency: locked.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: now},
				{Account: AccountDoctorPayable, Direction: Credit, AmountCents: locked.DoctorPayoutCents,
					Currency: locked.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: now},
				{Account: AccountProviderFeePayable, Direction: Credit, AmountCents: locked.ProviderFeeCents,
					Currency: locked.Currency, PaymentID: &pid, DoctorID: &did, OccurredAt: now},
			},
		}); err != nil {
			return err
		}
		return tx.Enqueue(ctx, events.SubjectPaymentSucceeded, locked.ID.String(),
			PaymentEvent{PaymentID: locked.ID, AmountCents: locked.AmountCents})
	}))

	settled, err := repo.GetPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, settled.Status)
	assert.Equal(t, 2, settled.Version)
	assert.True(t, settled.SplitBalances())
	require.NotNil(t, settled.CommissionRuleID)
	assert.Equal(t, rule.ID, *settled.CommissionRuleID)

	legs, err := repo.LedgerForPayment(ctx, p.ID)
	require.NoError(t, err)
	assert.Len(t, legs, 4)

	var outboxCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject = 'payment.succeeded'`).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount, "the event must have committed with the payment")

	// An optimistic lock conflict is detected, not silently ignored.
	stale := settled
	stale.Version = 1
	err = repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.UpdatePayment(ctx, &stale)
	})
	assert.ErrorIs(t, err, ErrVersionConflict)

	// An unbalanced ledger transaction is refused before it reaches the table.
	err = repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.WriteLedger(ctx, LedgerTransaction{
			EntryID: uuid.New(), EntryType: "bogus",
			Legs: []LedgerEntry{
				{Account: "a", Direction: Debit, AmountCents: 100, Currency: "LKR", OccurredAt: now},
				{Account: "b", Direction: Credit, AmountCents: 99, Currency: "LKR", OccurredAt: now},
			},
		})
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced")
}

// TestIntegrationOutboxRollsBackWithTheBusinessChange is ADR-005 in test form.
func TestIntegrationOutboxRollsBackWithTheBusinessChange(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	boom := fmt.Errorf("deliberate failure after the enqueue")
	p := Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 1000, GrossAmountCents: 1000, Currency: CurrencyLKR, Provider: ProviderStripe,
		Status: StatusPending, IdempotencyKey: uuid.NewString(),
	}
	err := repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.InsertPayment(ctx, &p); err != nil {
			return err
		}
		if err := tx.Enqueue(ctx, events.SubjectPaymentSucceeded, p.ID.String(), PaymentEvent{}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	var payments, outbox int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments`).Scan(&payments))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events`).Scan(&outbox))
	assert.Equal(t, 0, payments, "the payment must have rolled back")
	assert.Equal(t, 0, outbox, "and the event with it; there is no window where they disagree")
}

// TestIntegrationPayoutClaimIsExactlyOnceUnderConcurrency is the payout
// equivalent of the scheduling service's double-booking test.
//
// Twenty goroutines race to claim the same doctor's payments into twenty
// different payout batches. The `payout_id IS NULL` predicate must ensure every
// payment is claimed by exactly one of them.
func TestIntegrationPayoutClaimIsExactlyOnceUnderConcurrency(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	doctorID := uuid.New()
	const payments = 40
	const perPayout = 385_000
	succeededAt := time.Now().Add(-48 * time.Hour)

	for i := 0; i < payments; i++ {
		_, err := pool.Exec(ctx, `
			INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
			                      gross_amount_cents, provider,
			                      status, commission_cents, provider_fee_cents, doctor_payout_cents,
			                      idempotency_key, succeeded_at)
			VALUES ($1,$2,$3,$4,500000,500000,'stripe','succeeded',100000,15000,385000,$5,$6)`,
			uuid.New(), uuid.New(), uuid.New(), doctorID, uuid.NewString(), succeededAt)
		require.NoError(t, err)
	}

	const runners = 20
	cutoff := time.Now().Add(-24 * time.Hour)
	periodStart := time.Now().AddDate(0, 0, -2).Truncate(24 * time.Hour)
	periodEnd := periodStart.AddDate(0, 0, 1)

	type outcome struct {
		count int
		total int64
	}
	results := make([]outcome, runners)
	errsList := make([]error, runners)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errsList[i] = repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
				po := Payout{
					ID: uuid.New(), DoctorID: doctorID,
					// Each runner uses a distinct period so the
					// (doctor, period) unique index does not serialise them --
					// the point of the test is the claim predicate, not the
					// index.
					PeriodStart: periodStart.AddDate(0, 0, -i*2),
					PeriodEnd:   periodEnd.AddDate(0, 0, -i*2),
					Currency:    CurrencyLKR, Status: PayoutPending, Provider: ProviderStripe,
					IdempotencyKey: "payout:" + uuid.NewString(),
				}
				if err := tx.InsertPayout(ctx, &po); err != nil {
					return err
				}
				c, total, err := tx.ClaimPaymentsForPayout(ctx, po.ID, doctorID, cutoff)
				if err != nil {
					return err
				}
				results[i] = outcome{count: c, total: total}
				return nil
			})
		}(i)
	}
	close(start)
	wg.Wait()

	claimedTotal := 0
	var centsTotal int64
	for i := range results {
		require.NoError(t, errsList[i], "runner %d", i)
		claimedTotal += results[i].count
		centsTotal += results[i].total
	}

	assert.Equal(t, payments, claimedTotal,
		"every payment must be claimed exactly once across all concurrent runners")
	assert.Equal(t, int64(payments*perPayout), centsTotal)

	var unclaimed int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE payout_id IS NULL`).Scan(&unclaimed))
	assert.Equal(t, 0, unclaimed)

	// And no payment is in two payouts, which the schema makes structurally
	// impossible but which is worth asserting because it is the whole point.
	var distinctPayouts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT payout_id) FROM payments WHERE payout_id IS NOT NULL`).Scan(&distinctPayouts)) //nolint:errcheck
	assert.LessOrEqual(t, distinctPayouts, runners)
}

// TestIntegrationPayoutPeriodUniqueness proves two runs on the same night
// cannot both create a batch for the same doctor.
func TestIntegrationPayoutPeriodUniqueness(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	doctorID := uuid.New()
	start := time.Now().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)

	insert := func() error {
		return repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
			po := Payout{
				ID: uuid.New(), DoctorID: doctorID, PeriodStart: start, PeriodEnd: end,
				Currency: CurrencyLKR, Status: PayoutPending, Provider: ProviderStripe,
				IdempotencyKey: "payout:" + uuid.NewString(),
			}
			return tx.InsertPayout(ctx, &po)
		})
	}

	require.NoError(t, insert())
	err := insert()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDuplicate)
}

// TestIntegrationDoctorsWithDuePaymentsMatchesTheClaimPredicate guards against
// the two SQL predicates drifting apart, which would leave money permanently
// visible as "due" but never claimable.
func TestIntegrationDoctorsWithDuePaymentsMatchesTheClaimPredicate(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	doctorID := uuid.New()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	seed := func(status string, payout, refundedPayout int64, at time.Time) {
		_, err := pool.Exec(ctx, `
			INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
			                      gross_amount_cents, provider,
			                      status, commission_cents, provider_fee_cents, doctor_payout_cents,
			                      refunded_cents, refunded_commission_cents, refunded_payout_cents,
			                      idempotency_key, succeeded_at)
			VALUES ($1,$2,$3,$4,500000,500000,'stripe',$5,100000,15000,$6,$7,0,$8,$9,$10)`,
			uuid.New(), uuid.New(), uuid.New(), doctorID, status, payout,
			refundedPayout, refundedPayout, uuid.NewString(), at)
		require.NoError(t, err)
	}

	seed("succeeded", 385_000, 0, old)                // eligible
	seed("succeeded", 385_000, 0, recent)             // inside the hold
	seed("partially_refunded", 385_000, 185_000, old) // eligible, reduced
	seed("failed", 0, 0, old)                         // never eligible
	seed("succeeded", 385_000, 385_000, old)          // fully clawed back

	cutoff := time.Now().Add(-24 * time.Hour)
	dues, err := repo.DoctorsWithDuePayments(ctx, cutoff, 10)
	require.NoError(t, err)
	require.Len(t, dues, 1)
	assert.Equal(t, doctorID, dues[0].DoctorID)
	assert.Equal(t, 2, dues[0].PaymentCount)
	assert.Equal(t, int64(385_000+200_000), dues[0].AmountCents)

	var claimed int
	var total int64
	require.NoError(t, repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		po := Payout{
			ID: uuid.New(), DoctorID: doctorID,
			PeriodStart: cutoff.Truncate(24 * time.Hour), PeriodEnd: cutoff.Truncate(24*time.Hour).AddDate(0, 0, 1),
			Currency: CurrencyLKR, Status: PayoutPending, Provider: ProviderStripe,
			IdempotencyKey: "payout:" + uuid.NewString(),
		}
		if err := tx.InsertPayout(ctx, &po); err != nil {
			return err
		}
		var err error
		claimed, total, err = tx.ClaimPaymentsForPayout(ctx, po.ID, doctorID, cutoff)
		return err
	}))

	assert.Equal(t, dues[0].PaymentCount, claimed,
		"the due query and the claim query must agree about what is eligible")
	assert.Equal(t, dues[0].AmountCents, total)
}

// TestIntegrationEndToEndWebhookIdempotency runs the real service against the
// real database, delivering the same event ten times.
func TestIntegrationEndToEndWebhookIdempotency(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	rule := CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	_, err := repo.SeedRules(ctx, []CommissionRule{rule})
	require.NoError(t, err)

	prov := newTestProvider(ProviderMock)
	reg := NewRegistry(ProviderMock)
	reg.Register(prov)

	pricer := NewPricer(repo, time.Minute, CommissionRule{})
	require.NoError(t, pricer.Refresh(ctx))

	svc := NewService(repo, pricer, reg, zerolog.Nop(), Config{
		HoldPeriod: 24 * time.Hour, DefaultProvider: ProviderMock, Currency: CurrencyLKR,
	})

	appointmentID := uuid.New()
	require.NoError(t, svc.OnAppointmentCreated(ctx, events.AppointmentCreated{
		AppointmentID: appointmentID, PatientID: uuid.New(), DoctorID: uuid.New(),
		AmountCents: 500_000, Currency: CurrencyLKR, Specialty: "GP",
		StartAt: time.Now().Add(48 * time.Hour),
	}))

	created, err := repo.GetPaymentByAppointment(ctx, appointmentID)
	require.NoError(t, err)

	view, err := svc.CreateIntent(ctx, CreateIntentInput{
		AppointmentID: appointmentID, CallerID: created.PatientID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, view.Payment.ProviderIntentID)

	prov.setEvent(WebhookEvent{
		EventID: "evt_integration", Type: "payment.succeeded",
		Outcome: OutcomePaymentSucceeded, ProviderIntentID: view.Payment.ProviderIntentID,
		Raw: []byte(`{"id":"evt_integration"}`),
	})

	for i := 0; i < 10; i++ {
		res, err := svc.HandleWebhook(ctx, ProviderMock, nil, []byte(`{"id":"evt_integration"}`))
		require.NoError(t, err)
		assert.Equal(t, i > 0, res.Replayed)
	}

	settled, err := repo.GetPayment(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, settled.Status)
	assert.True(t, settled.SplitBalances())
	assert.Equal(t, int64(100_000), settled.CommissionCents)
	assert.Equal(t, int64(15_000), settled.ProviderFeeCents)
	assert.Equal(t, int64(385_000), settled.DoctorPayoutCents)

	legs, err := repo.LedgerForPayment(ctx, created.ID)
	require.NoError(t, err)
	assert.Len(t, legs, 4, "ten deliveries produced exactly one four-leg capture")

	var webhookRows, outboxRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_events`).Scan(&webhookRows))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE subject = 'payment.succeeded'`).Scan(&outboxRows))
	assert.Equal(t, 1, webhookRows)
	assert.Equal(t, 1, outboxRows)
}

// --- promotional codes ------------------------------------------------------

// promoIntegrationService builds a service on the real repository. The rail is
// irrelevant to these tests -- nothing here calls a provider -- but the
// registry must not be empty or NewService cannot resolve a default.
func promoIntegrationService(t *testing.T, pool *pgxpool.Pool) (*Service, *Repository) {
	t.Helper()
	ctx := context.Background()

	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))
	rule := CommissionRule{
		ID: uuid.New(), RuleKey: "default", Version: 1, Scope: ScopeDefault,
		RateBps: 2000, ProviderFeeBps: 300, Rounding: RoundHalfUp,
		EffectiveFrom: time.Now().Add(-time.Hour),
	}
	if _, err := repo.SeedRules(ctx, []CommissionRule{rule}); err != nil {
		t.Fatalf("seed rules: %v", err)
	}

	pricer := NewPricer(repo, time.Minute, rule)
	require.NoError(t, pricer.Refresh(ctx))

	reg := NewRegistry(ProviderStripe)
	reg.Register(&integrationNoopRail{})

	svc := NewService(repo, pricer, reg, zerolog.Nop(), Config{
		DefaultProvider: ProviderStripe,
		Currency:        CurrencyLKR,
		Promo:           PromoConfig{ReservationTTL: 30 * time.Minute},
	})
	return svc, repo
}

// integrationNoopRail satisfies the registry. It is never called: the promo
// path touches no provider, which is itself worth knowing -- applying a code
// must not depend on Stripe being reachable.
type integrationNoopRail struct{}

func (integrationNoopRail) Name() string { return string(ProviderStripe) }
func (integrationNoopRail) CreateIntent(context.Context, IntentRequest) (IntentResult, error) {
	return IntentResult{}, ErrUnsupported
}

func (integrationNoopRail) VerifyWebhook(context.Context, http.Header, []byte) (WebhookEvent, error) {
	return WebhookEvent{}, ErrSignatureInvalid
}

func (integrationNoopRail) Capture(context.Context, CaptureRequest) (CaptureResult, error) {
	return CaptureResult{}, ErrUnsupported
}

func (integrationNoopRail) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return RefundResult{}, ErrUnsupported
}

func (integrationNoopRail) Payout(context.Context, PayoutRequest) (PayoutResult, error) {
	return PayoutResult{}, ErrUnsupported
}

// seedPromoPayment inserts a pending, un-quoted payment straight through SQL.
func seedPromoPayment(t *testing.T, pool *pgxpool.Pool, patientID uuid.UUID, amount int64) Payment {
	t.Helper()
	p := Payment{
		ID: uuid.New(), AppointmentID: uuid.New(), PatientID: patientID, DoctorID: uuid.New(),
		AmountCents: amount, GrossAmountCents: amount, Currency: CurrencyLKR,
		Provider: ProviderStripe, Status: StatusPending,
		IdempotencyKey: "appointment:" + uuid.NewString(),
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
		                      gross_amount_cents, currency, provider, status, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$5,$6,$7,$8,$9)`,
		p.ID, p.AppointmentID, p.PatientID, p.DoctorID, p.AmountCents,
		p.Currency, p.Provider, p.Status, p.IdempotencyKey)
	require.NoError(t, err)
	return p
}

// TestIntegrationPromoConcurrentRedemption is the money test for promotions,
// and the reason the promo unit tests do not attempt concurrency.
//
// One hundred patients, each with their own booking, all type the same code at
// the same instant. The code has exactly ONE redemption. Exactly one of them
// may get it, the counter must end at exactly one, and there must be exactly
// one live redemption row -- not ninety-nine failures and two winners because
// two goroutines read the count before either wrote.
//
// This cannot be tested against the in-memory fake: fakeStore.InTx holds a
// mutex for the whole transaction body, so it would pass whether or not
// SELECT ... FOR UPDATE existed. Only real Postgres, with real row locks and
// real MVCC snapshots, says anything.
func TestIntegrationPromoConcurrentRedemption(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	svc, repo := promoIntegrationService(t, pool)

	code, err := svc.CreatePromoCode(ctx, CreatePromoCodeInput{
		Code:           "ONLYONE",
		DiscountType:   PromoFixed,
		AmountOffCents: 50_000,
		MaxRedemptions: func() *int { n := 1; return &n }(),
		MaxPerUser:     1,
	})
	require.NoError(t, err)

	const runners = 100
	payments := make([]Payment, runners)
	for i := range payments {
		payments[i] = seedPromoPayment(t, pool, uuid.New(), 250_000)
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, runners)
	)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = svc.ApplyPromo(ctx, ApplyPromoInput{
				AppointmentID: payments[i].AppointmentID,
				CallerID:      payments[i].PatientID,
				Code:          "onlyone",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	winners, exhausted, other := 0, 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrPromoExhausted):
			exhausted++
		default:
			other++
			t.Errorf("runner %d failed with an unexpected error: %v", i, err)
		}
	}

	assert.Equal(t, 1, winners, "exactly one patient may redeem a code with one redemption")
	assert.Equal(t, runners-1, exhausted, "every loser must be told the code is used up, not handed a 500")
	assert.Equal(t, 0, other)

	reloaded, err := repo.GetPromoCodeByCode(ctx, code.Code)
	require.NoError(t, err)
	assert.Equal(t, 1, reloaded.RedemptionCount, "the counter must not double-spend")

	var live int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM promo_redemptions WHERE promo_code_id = $1 AND status <> 'released'`,
		code.ID).Scan(&live))
	assert.Equal(t, 1, live, "exactly one live redemption row")

	// And exactly one payment was actually discounted. This is the assertion
	// that would catch a counter that stayed correct while two payments were
	// repriced anyway.
	var discounted int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE discount_cents > 0`).Scan(&discounted))
	assert.Equal(t, 1, discounted, "exactly one booking may have been repriced")

	var mismatched int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE gross_amount_cents <> amount_cents + discount_cents`).Scan(&mismatched))
	assert.Equal(t, 0, mismatched, "the discount invariant must hold on every row")
}

// TestIntegrationPromoConcurrentPerUserLimit is the same race from the other
// direction: ONE patient, ten simultaneous applications of a code that allows
// one per user, against ten of their own bookings.
//
// The global budget is unlimited here, so the only thing that can stop the
// tenth redemption is the per-user check -- and that check reads a count and
// then writes, which is only safe because the code's row lock is held across
// both.
func TestIntegrationPromoConcurrentPerUserLimit(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	svc, _ := promoIntegrationService(t, pool)

	_, err := svc.CreatePromoCode(ctx, CreatePromoCodeInput{
		Code: "ONEPERPERSON", DiscountType: PromoFixed, AmountOffCents: 25_000, MaxPerUser: 1,
	})
	require.NoError(t, err)

	patient := uuid.New()
	const runners = 10
	payments := make([]Payment, runners)
	for i := range payments {
		payments[i] = seedPromoPayment(t, pool, patient, 250_000)
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, runners)
	)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = svc.ApplyPromo(ctx, ApplyPromoInput{
				AppointmentID: payments[i].AppointmentID,
				CallerID:      patient,
				Code:          "ONEPERPERSON",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrPromoExhausted):
		default:
			t.Errorf("runner %d failed with an unexpected error: %v", i, err)
		}
	}
	assert.Equal(t, 1, winners, "one per user means one, however many bookings are tried at once")

	var discounted int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE patient_id = $1 AND discount_cents > 0`, patient).Scan(&discounted))
	assert.Equal(t, 1, discounted)
}

// TestIntegrationPromoSwapDoesNotDeadlock.
//
// Replacing code X with code Y locks two promo_codes rows. Two transactions
// swapping the same pair in opposite directions is the textbook deadlock, and
// Postgres would abort one of them with SQLSTATE 40P01 -- surfacing to a
// patient as a 500 on the payment screen. lockCodesInOrder sorts by id so both
// transactions always take the same row first.
func TestIntegrationPromoSwapDoesNotDeadlock(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	svc, _ := promoIntegrationService(t, pool)

	for _, code := range []string{"ALPHA", "BETA"} {
		_, err := svc.CreatePromoCode(ctx, CreatePromoCodeInput{
			Code: code, DiscountType: PromoFixed, AmountOffCents: 20_000, MaxPerUser: 100,
		})
		require.NoError(t, err)
	}

	const pairs = 25
	first := make([]Payment, pairs)
	second := make([]Payment, pairs)
	for i := 0; i < pairs; i++ {
		first[i] = seedPromoPayment(t, pool, uuid.New(), 250_000)
		second[i] = seedPromoPayment(t, pool, uuid.New(), 250_000)

		// Half start on ALPHA and half on BETA, so the swaps below genuinely
		// cross.
		_, err := svc.ApplyPromo(ctx, ApplyPromoInput{
			AppointmentID: first[i].AppointmentID, CallerID: first[i].PatientID, Code: "ALPHA",
		})
		require.NoError(t, err)
		_, err = svc.ApplyPromo(ctx, ApplyPromoInput{
			AppointmentID: second[i].AppointmentID, CallerID: second[i].PatientID, Code: "BETA",
		})
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, pairs*2)
	for i := 0; i < pairs; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i*2] = svc.ApplyPromo(ctx, ApplyPromoInput{
				AppointmentID: first[i].AppointmentID, CallerID: first[i].PatientID, Code: "BETA",
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i*2+1] = svc.ApplyPromo(ctx, ApplyPromoInput{
				AppointmentID: second[i].AppointmentID, CallerID: second[i].PatientID, Code: "ALPHA",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "swap %d deadlocked or failed", i)
	}

	// Every payment ends holding exactly one code, and the counters agree with
	// the rows.
	for _, code := range []string{"ALPHA", "BETA"} {
		stored, err := svc.store.(*Repository).GetPromoCodeByCode(ctx, code)
		require.NoError(t, err)

		var live int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM promo_redemptions WHERE promo_code_id = $1 AND status <> 'released'`,
			stored.ID).Scan(&live))
		assert.Equalf(t, live, stored.RedemptionCount,
			"%s: redemption_count (%d) must equal the live rows (%d)", code, stored.RedemptionCount, live)
	}

	var stacked int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT payment_id FROM promo_redemptions WHERE status <> 'released'
			GROUP BY payment_id HAVING COUNT(*) > 1
		) t`).Scan(&stacked))
	assert.Equal(t, 0, stacked, "no payment may hold two codes at once")
}

// TestIntegrationPromoBudgetConstraintIsTheBackstop proves the database would
// still refuse an over-spend if the service ever lost its row lock. The
// constraint is not the mechanism -- the lock is -- but a constraint that has
// never been fired is a constraint nobody knows is spelled correctly.
func TestIntegrationPromoBudgetConstraintIsTheBackstop(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()

	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO promo_codes (id, code, discount_type, amount_off_cents, max_redemptions, redemption_count)
		VALUES ($1, 'CAPPED', 'fixed', 10000, 2, 2)`, id)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE promo_codes SET redemption_count = 3 WHERE id = $1`, id)
	require.Error(t, err, "the database must refuse a third redemption on a code capped at two")

	_, err = pool.Exec(ctx, `UPDATE promo_codes SET redemption_count = -1 WHERE id = $1`, id)
	require.Error(t, err, "and must refuse a negative count")
}

// TestIntegrationOnePendingPINChallengePerPayment. The partial unique index is
// what stops a resend leaving two live references, either of which the
// patient's SMS might correspond to.
func TestIntegrationOnePendingPINChallengePerPayment(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	p := seedPromoPayment(t, pool, uuid.New(), 250_000)

	insert := func(status string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO pin_challenges (id, payment_id, provider, reference, status, expires_at)
			VALUES ($1,$2,'dialog',$3,$4,NOW() + interval '5 minutes')`,
			uuid.New(), p.ID, "ref_"+uuid.NewString(), status)
		return err
	}

	require.NoError(t, insert("pending"))
	require.Error(t, insert("pending"), "two pending challenges for one payment must be refused")
	// A superseded one alongside a pending one is exactly the resend case.
	require.NoError(t, insert("superseded"))

	_, err := pool.Exec(ctx, `
		INSERT INTO pin_challenges (id, payment_id, provider, reference, status, attempts, max_attempts, expires_at)
		VALUES ($1,$2,'dialog','ref_over','pending',4,3,NOW())`, uuid.New(), p.ID)
	require.Error(t, err, "attempts above max_attempts must be refused by the database too")
}

// TestIntegrationOneDefaultPaymentMethodPerPatient. Two defaults is not
// cosmetic: checkout would charge whichever row came back first.
func TestIntegrationOneDefaultPaymentMethodPerPatient(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	patient := uuid.New()

	insert := func(token string, isDefault bool, last4 string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO payment_methods (id, patient_id, provider, provider_token, brand, last4, is_default)
			VALUES ($1,$2,'stripe',$3,'visa',$4,$5)`,
			uuid.New(), patient, token, last4, isDefault)
		return err
	}

	require.NoError(t, insert("pm_1", true, "4242"))
	require.Error(t, insert("pm_2", true, "5555"), "a second default for one patient must be refused")
	require.NoError(t, insert("pm_3", false, "6011"))

	require.Error(t, insert("pm_1", false, "4242"), "the same token twice is the same card")

	// The PAN guard, at the database this time.
	require.Error(t, insert("pm_4", false, "4242424242424242"),
		"last4 must be exactly four digits; anything longer could be a card number")
	require.Error(t, insert("pm_5", false, "42x2"), "last4 must be digits")
}

// --- F19: the payout batch race ---------------------------------------------

// payoutRail is a settlement rail with the two properties that make the race
// visible: a transfer takes time, and the idempotency key is honoured exactly
// as Stripe honours it.
//
// It also records the one thing no assertion about the database can see -- the
// amount that actually left the building -- so a test can compare it against
// what the ledger claims was settled.
type payoutRail struct {
	mu    sync.Mutex
	delay time.Duration

	// transfers is idempotency key -> amount actually sent.
	transfers map[string]int64
	// ids is idempotency key -> transfer id, so a retry returns the original.
	ids map[string]string
	// mismatchedRetries counts requests that presented an idempotency key
	// already used, for a DIFFERENT amount. Every one of those is a batch that
	// changed after its transfer was made.
	mismatchedRetries int
}

func newPayoutRail(delay time.Duration) *payoutRail {
	return &payoutRail{delay: delay, transfers: map[string]int64{}, ids: map[string]string{}}
}

func (r *payoutRail) Name() string { return string(ProviderStripe) }

func (r *payoutRail) CreateIntent(context.Context, IntentRequest) (IntentResult, error) {
	return IntentResult{}, ErrUnsupported
}

func (r *payoutRail) VerifyWebhook(context.Context, http.Header, []byte) (WebhookEvent, error) {
	return WebhookEvent{}, ErrSignatureInvalid
}

func (r *payoutRail) Capture(context.Context, CaptureRequest) (CaptureResult, error) {
	return CaptureResult{}, ErrUnsupported
}

func (r *payoutRail) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return RefundResult{}, ErrUnsupported
}

func (r *payoutRail) Payout(_ context.Context, req PayoutRequest) (PayoutResult, error) {
	r.mu.Lock()
	if id, ok := r.ids[req.IdempotencyKey]; ok {
		if r.transfers[req.IdempotencyKey] != req.AmountCents {
			// Stripe would answer with the original transfer here too. That is
			// the point: the second amount is silently never sent, and the
			// caller has no way to notice unless it asks.
			r.mismatchedRetries++
		}
		r.mu.Unlock()
		return PayoutResult{TransferID: id, Status: PayoutPaid}, nil
	}
	id := "tr_" + uuid.NewString()
	r.ids[req.IdempotencyKey] = id
	r.transfers[req.IdempotencyKey] = req.AmountCents
	r.mu.Unlock()

	// Outside the lock, and outside any database transaction: this is the
	// window in which the batch used to be mutated underneath us.
	time.Sleep(r.delay)
	return PayoutResult{TransferID: id, Status: PayoutPaid}, nil
}

func (r *payoutRail) totalTransferred() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, amount := range r.transfers {
		total += amount
	}
	return total
}

func (r *payoutRail) distinctTransfers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

func (r *payoutRail) mismatched() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mismatchedRetries
}

// seedSettleablePayment inserts one captured payment together with the balanced
// capture ledger it would have produced, so liability:doctor_payable carries a
// real credit for it. Without the credit the "nets to zero" assertion would be
// vacuous.
func seedSettleablePayment(t *testing.T, pool *pgxpool.Pool, doctorID uuid.UUID, succeededAt time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	const (
		amount     = int64(500_000)
		commission = int64(100_000)
		fee        = int64(15_000)
		payout     = int64(385_000)
	)

	paymentID := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO payments (id, appointment_id, patient_id, doctor_id, amount_cents,
		                      gross_amount_cents, currency, provider, status,
		                      commission_cents, provider_fee_cents, doctor_payout_cents,
		                      idempotency_key, succeeded_at)
		VALUES ($1,$2,$3,$4,$5,$5,'LKR','stripe','succeeded',$6,$7,$8,$9,$10)`,
		paymentID, uuid.New(), uuid.New(), doctorID, amount,
		commission, fee, payout, uuid.NewString(), succeededAt)
	require.NoError(t, err)

	entryID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO ledger_entries (entry_id, entry_type, account, direction, amount_cents,
		                            currency, payment_id, doctor_id, occurred_at)
		VALUES ($1,'payment.captured','asset:provider_receivable','debit',$2,'LKR',$6,$7,$8),
		       ($1,'payment.captured','revenue:commission','credit',$3,'LKR',$6,$7,$8),
		       ($1,'payment.captured','liability:provider_fee_payable','credit',$4,'LKR',$6,$7,$8),
		       ($1,'payment.captured','liability:doctor_payable','credit',$5,'LKR',$6,$7,$8)`,
		entryID, amount, commission, fee, payout, paymentID, doctorID, succeededAt)
	require.NoError(t, err)

	return paymentID
}

// doctorPayableNet returns credits minus debits on liability:doctor_payable.
//
// It is the single number that says whether the platform's books agree with
// what it actually paid. Positive means doctors are still owed money that was
// captured for them; negative means the ledger says we paid more than we owed,
// which in an append-only table cannot be corrected, only annotated.
func doctorPayableNet(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var net int64
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(CASE direction WHEN 'credit' THEN amount_cents ELSE -amount_cents END), 0)
		FROM ledger_entries WHERE account = $1`, AccountDoctorPayable).Scan(&net))
	return net
}

// payoutLedgerDebits returns everything the ledger says was settled out of
// liability:doctor_payable by the payout job.
func payoutLedgerDebits(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var total int64
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(amount_cents), 0) FROM ledger_entries
		WHERE account = $1 AND direction = 'debit' AND entry_type = $2`,
		AccountDoctorPayable, EntryPayoutSent).Scan(&total))
	return total
}

// TestIntegrationConcurrentPayoutRunsPayEachDoctorExactlyOnce is the money test
// for F19.
//
// The cron scheduler starts in EVERY replica, so at 02:00 all N pods call
// Run() at once, and POST /api/v1/payouts/run can fire on top of that. The
// race that produced the finding needs three things at the same time, all of
// which are reproduced here rather than described:
//
//   - a batch whose transfer is in flight, so its row says `processing` while
//     no database lock is held (the transfer is deliberately outside the
//     transaction; holding row locks across a rail's latency is worse);
//   - a second replica reaching prepareBatch during that window;
//   - payments that became eligible in between, because each replica computes
//     its own hold cutoff from its own clock, and minutes pass between the
//     cron firing and an operator pressing the button.
//
// Under the defect the second replica grew the in-flight batch, the first
// replica then wrote ledger legs for the larger amount against a transfer of
// the smaller one, and the difference was stamped with a payout_id so it could
// never be claimed again. The doctor is underpaid and ledger_entries -- which
// is append-only by database trigger -- disagrees with the bank.
//
// The assertions are therefore about money, not about status columns:
//
//	every payment settles exactly once
//	what the rail was asked to send == what the ledger says was settled
//	liability:doctor_payable nets to zero
//
// No lease is configured, deliberately. Correctness must come from Postgres;
// the Redis lease in production is an optimisation and this test is what says
// so.
func TestIntegrationConcurrentPayoutRunsPayEachDoctorExactlyOnce(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	const (
		doctors    = 2
		runners    = 12
		perDoctor  = runners // payment j becomes eligible for runner j onwards
		payoutEach = int64(385_000)
		hold       = 24 * time.Hour
	)

	// Noon, so that the 24-hour truncation that derives the settlement period
	// lands on the same day for every runner's cutoff.
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	doctorIDs := make([]uuid.UUID, doctors)
	dest := StaticDestinations{}
	for d := range doctorIDs {
		doctorIDs[d] = uuid.New()
		dest[doctorIDs[d]] = fmt.Sprintf("acct_%d", d)
		for j := 0; j < perDoctor; j++ {
			// Runner i's cutoff is base + i minutes - hold, so this payment is
			// eligible for runner i exactly when j <= i.
			succeededAt := base.Add(-hold).Add(time.Duration(j) * time.Minute).Add(-30 * time.Second)
			seedSettleablePayment(t, pool, doctorIDs[d], succeededAt)
		}
	}

	owed := int64(doctors) * int64(perDoctor) * payoutEach
	require.Equal(t, owed, doctorPayableNet(t, pool), "every seeded capture must credit the doctor payable")

	// The transfer is slow enough that every later runner reaches prepareBatch
	// while the first batch is still in flight.
	rail := newPayoutRail(300 * time.Millisecond)
	reg := NewRegistry(ProviderStripe)
	reg.Register(rail)

	newRunner := func(at time.Time) *PayoutRunner {
		return NewPayoutRunner(repo, reg, dest, zerolog.Nop(), PayoutConfig{
			HoldPeriod:       hold,
			Provider:         ProviderStripe,
			MaxDoctorsPerRun: 100,
		}).WithClock(func() time.Time { return at })
	}

	errsList := make([]error, runners)
	var wg sync.WaitGroup
	begin := make(chan struct{})
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			// Staggered by well under the transfer delay, so the whole field
			// is inside the first batch's in-flight window.
			time.Sleep(time.Duration(i) * 15 * time.Millisecond)
			_, err := newRunner(base.Add(time.Duration(i) * time.Minute)).Run(ctx)
			errsList[i] = err
		}(i)
	}
	close(begin)
	wg.Wait()

	for i, err := range errsList {
		require.NoError(t, err, "runner %d", i)
	}

	// Some payments were deferred rather than claimed, which is what makes the
	// drain below non-vacuous: the burst did not simply settle everything in
	// one pass, so the deferral path is genuinely exercised.
	var deferred int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE payout_id IS NULL`).Scan(&deferred))
	assert.Positive(t, deferred,
		"the race window must have been reached; with everything settled in one pass this test proves nothing")

	// Nothing was transferred twice for two different amounts, which is what a
	// batch changing under an in-flight transfer looks like from the rail's
	// side.
	assert.Zero(t, rail.mismatched(),
		"a transfer was re-requested under the same idempotency key for a different amount")

	// Drain: each subsequent day's run settles what the burst deferred. A
	// payout row is UNIQUE (doctor_id, period_start, period_end), so deferred
	// payments genuinely do wait for the next period -- exactly what the
	// already-paid branch has always done.
	for day := 1; day <= perDoctor+2; day++ {
		summary, err := newRunner(base.AddDate(0, 0, day)).Run(ctx)
		require.NoError(t, err)
		if summary.Created == 0 && summary.Paid == 0 && summary.Resumed == 0 {
			break
		}
	}

	// --- every payment settled exactly once
	var unclaimed, unsettled int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE payout_id IS NULL`).Scan(&unclaimed))
	assert.Zero(t, unclaimed, "every captured payment must end up in a payout batch")
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM payments p
		JOIN payouts o ON o.id = p.payout_id
		WHERE o.status <> 'paid'`).Scan(&unsettled))
	assert.Zero(t, unsettled, "no payment may be stamped into a batch that never paid")

	// --- what was sent is what the books say was settled
	assert.Equal(t, owed, rail.totalTransferred(),
		"the doctors must be sent exactly what they were owed: no more, and no less")
	assert.Equal(t, rail.totalTransferred(), payoutLedgerDebits(t, pool),
		"the ledger must record the amount that actually left the building")

	// --- and the liability account closes
	assert.Zero(t, doctorPayableNet(t, pool),
		"liability:doctor_payable must net to zero once every captured payment has been settled")

	// --- one transfer per batch, and every batch balances against its rows
	var batches int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payouts WHERE status = 'paid'`).Scan(&batches))
	assert.Equal(t, batches, rail.distinctTransfers(),
		"each settled batch corresponds to exactly one transfer")

	rows, err := pool.Query(ctx, `
		SELECT o.id, o.doctor_id, o.amount_cents, o.payment_count,
		       COALESCE(SUM(p.doctor_payout_cents - p.refunded_payout_cents), 0),
		       COUNT(p.id)
		FROM payouts o LEFT JOIN payments p ON p.payout_id = o.id
		WHERE o.status = 'paid'
		GROUP BY o.id`)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id, doctorID uuid.UUID
		var batchAmount, rowAmount int64
		var batchCount, rowCount int
		require.NoError(t, rows.Scan(&id, &doctorID, &batchAmount, &batchCount, &rowAmount, &rowCount))
		assert.Equal(t, rowAmount, batchAmount, "payout %s claims %d but its payments total %d", id, batchAmount, rowAmount)
		assert.Equal(t, rowCount, batchCount, "payout %s claims %d payments but holds %d", id, batchCount, rowCount)
		seen++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, batches, seen)
}

// TestIntegrationConcurrentPayoutRunsTransferOncePerDoctor is the same job
// under the simpler shape: every payment already eligible, every replica's
// clock identical, all N firing at the same instant. Exactly one batch per
// doctor may exist and exactly one transfer per doctor may be made.
func TestIntegrationConcurrentPayoutRunsTransferOncePerDoctor(t *testing.T) {
	pool := freshSchema(t)
	ctx := context.Background()
	repo := NewRepository(pool, events.NewOutbox("telemed-payment-service"))

	const (
		doctors    = 3
		perDoctor  = 8
		runners    = 16
		payoutEach = int64(385_000)
		hold       = 24 * time.Hour
	)

	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	dest := StaticDestinations{}
	for d := 0; d < doctors; d++ {
		doctorID := uuid.New()
		dest[doctorID] = fmt.Sprintf("acct_%d", d)
		for j := 0; j < perDoctor; j++ {
			seedSettleablePayment(t, pool, doctorID, base.Add(-48*time.Hour))
		}
	}

	owed := int64(doctors) * int64(perDoctor) * payoutEach
	rail := newPayoutRail(150 * time.Millisecond)
	reg := NewRegistry(ProviderStripe)
	reg.Register(rail)

	var wg sync.WaitGroup
	begin := make(chan struct{})
	errsList := make([]error, runners)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			_, errsList[i] = NewPayoutRunner(repo, reg, dest, zerolog.Nop(), PayoutConfig{
				HoldPeriod: hold, Provider: ProviderStripe, MaxDoctorsPerRun: 100,
			}).WithClock(func() time.Time { return base }).Run(ctx)
		}(i)
	}
	close(begin)
	wg.Wait()

	for i, err := range errsList {
		require.NoError(t, err, "runner %d", i)
	}

	var batches int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payouts WHERE status = 'paid'`).Scan(&batches))
	assert.Equal(t, doctors, batches, "one settled batch per doctor, whatever the replica count")
	assert.Equal(t, doctors, rail.distinctTransfers(), "each doctor is paid exactly once")
	assert.Equal(t, owed, rail.totalTransferred())
	assert.Equal(t, owed, payoutLedgerDebits(t, pool))
	assert.Zero(t, rail.mismatched())
	assert.Zero(t, doctorPayableNet(t, pool),
		"liability:doctor_payable must net to zero")

	var unclaimed int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE payout_id IS NULL`).Scan(&unclaimed))
	assert.Zero(t, unclaimed)
}
