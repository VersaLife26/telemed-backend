package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// The payout job settles doctors. It runs at 02:00 Asia/Colombo every day and
// its single hard requirement is that a crash anywhere in the middle, followed
// by a re-run, pays nobody twice.
//
// That property comes from three things and no others:
//
//  1. A payout row is UNIQUE (doctor_id, period_start, period_end). Two runs
//     for the same night cannot both create a batch.
//  2. Payments are claimed by `UPDATE ... WHERE payout_id IS NULL`. A payment
//     can be claimed exactly once, ever, by anyone.
//  3. The provider transfer carries the payout id as its idempotency key, so a
//     retry after an ambiguous timeout returns the original transfer rather
//     than making a second one.
//
// The job is therefore a resume, not a restart: it first finishes every payout
// left unsettled by any previous run, then creates tonight's.

// DestinationResolver maps a doctor to the account handle their rail settles
// to -- a Stripe Connect account id, a PayHere merchant reference.
//
// This service does not and must not store bank details; it stores the opaque
// handle the rail issued. In a full deployment this is a lookup against
// doctor-service. The static implementation below is what makes the job
// runnable today without inventing a dependency that does not exist yet.
type DestinationResolver interface {
	PayoutAccount(ctx context.Context, doctorID uuid.UUID) (string, error)
}

// ErrNoDestination means the doctor has no settlement account on file. The
// payout is marked failed with that reason and the money stays claimed, so the
// operator can fix the account and re-run rather than hunting for lost funds.
var ErrNoDestination = errors.New("payment: doctor has no payout account configured")

// ErrPayoutMutatedInFlight means a payout batch changed between the moment its
// transfer was made and the moment the outcome was recorded.
//
// prepareBatch refuses to claim into anything but a PENDING batch, so this
// cannot happen through the job. It is asserted anyway, immediately before the
// ledger legs are appended, because the ledger is append-only: a leg written
// for the wrong amount is permanent, and "the invariant held when we wrote the
// code" is not the same claim as "the invariant held when we wrote the row".
//
// The batch is deliberately left `processing` rather than forced to some
// invented state. It is the one payout an operator genuinely has to look at,
// and the transfer has already been made -- guessing here would be worse than
// stopping.
var ErrPayoutMutatedInFlight = errors.New("payment: payout batch changed while its transfer was in flight")

// ErrPayoutRunInProgress means another replica holds the run lease. It is not
// a failure: the settlement is happening, just not here.
var ErrPayoutRunInProgress = errors.New("payment: another replica is running the payout job")

// StaticDestinations resolves from a configured map. It is the real
// implementation for dev and staging, and the fallback in production for
// doctors onboarded before the doctor-service lookup exists.
type StaticDestinations map[uuid.UUID]string

// PayoutAccount implements DestinationResolver.
func (m StaticDestinations) PayoutAccount(_ context.Context, doctorID uuid.UUID) (string, error) {
	if acct, ok := m[doctorID]; ok && acct != "" {
		return acct, nil
	}
	return "", fmt.Errorf("%w: doctor %s", ErrNoDestination, doctorID)
}

// Lease is the slice of cache.Cache the payout job needs: a token-guarded
// distributed lock. It is declared here rather than taking cache.Cache whole
// because a settlement job has no business being handed Get, Set, Del and a
// sorted-set API it will never call.
//
// cache.Cache satisfies it as-is.
type Lease interface {
	Lock(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error)
	Unlock(ctx context.Context, key, token string) error
}

// PayoutLockKey is the lease every replica contends for before running the
// settlement job.
const PayoutLockKey = "payment:payout:run"

// PayoutConfig tunes the job.
type PayoutConfig struct {
	// HoldPeriod is how long a captured payment must age before settlement.
	HoldPeriod time.Duration
	// Provider is the rail payouts are made over.
	Provider ProviderName
	// MaxDoctorsPerRun bounds one run so a backlog cannot become a six-hour
	// transaction.
	MaxDoctorsPerRun int
	// Schedule is a cron expression in the business timezone.
	Schedule string
	// Timezone is the business timezone the schedule is interpreted in.
	Timezone string
	// LeaseTTL bounds how long one replica may hold the run lease. It must
	// exceed a realistic run, because a lease that expires mid-run puts a
	// second replica into exactly the concurrency this fix exists to remove.
	LeaseTTL time.Duration
}

// PayoutRunner executes the settlement job.
type PayoutRunner struct {
	store        Store
	providers    *Registry
	destinations DestinationResolver
	log          zerolog.Logger
	cfg          PayoutConfig
	lease        Lease
	now          func() time.Time
}

// NewPayoutRunner builds the job.
func NewPayoutRunner(store Store, providers *Registry, dest DestinationResolver, log zerolog.Logger, cfg PayoutConfig) *PayoutRunner {
	if cfg.HoldPeriod <= 0 {
		cfg.HoldPeriod = 24 * time.Hour
	}
	if cfg.MaxDoctorsPerRun <= 0 {
		cfg.MaxDoctorsPerRun = 500
	}
	if cfg.Schedule == "" {
		cfg.Schedule = "0 2 * * *" // 02:00 daily
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Asia/Colombo"
	}
	if cfg.Provider == "" {
		cfg.Provider = providers.Default()
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 15 * time.Minute
	}
	return &PayoutRunner{store: store, providers: providers, destinations: dest, log: log, cfg: cfg, now: time.Now}
}

// WithLease gives the runner a distributed lease so that, of N replicas whose
// cron fires at the same instant, only one settles.
//
// It is an optimisation and nothing more, and the concurrency test proves that
// by running N goroutines with no lease at all. Correctness comes from the
// database: UNIQUE (doctor_id, period_start, period_end) on payouts,
// `payout_id IS NULL` on the claim, `FOR UPDATE` on the batch, and the
// pending-only rule in prepareBatch. A lease built on a cache with a TTL is
// not a mutex -- Redis can lose the key on failover -- so nothing here is
// allowed to depend on it. What it buys is that the normal case does not
// generate contention, retries and duplicate provider calls every night.
func (r *PayoutRunner) WithLease(l Lease) *PayoutRunner {
	r.lease = l
	return r
}

// WithClock pins the clock. Tests only.
func (r *PayoutRunner) WithClock(now func() time.Time) *PayoutRunner {
	r.now = now
	return r
}

// RunSummary reports what one execution did.
type RunSummary struct {
	Resumed     int       `json:"resumed"`
	Created     int       `json:"created"`
	Paid        int       `json:"paid"`
	Failed      int       `json:"failed"`
	Skipped     int       `json:"skipped"`
	AmountCents int64     `json:"amount_cents"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// Start schedules the job and blocks until ctx is cancelled.
//
// The schedule is evaluated in Asia/Colombo through tzdata rather than as a
// fixed +05:30 offset. Sri Lanka has changed its standard time three times
// since 1996; hardcoding the offset is a bet that it never happens again.
func (r *PayoutRunner) Start(ctx context.Context) error {
	loc, err := time.LoadLocation(r.cfg.Timezone)
	if err != nil {
		return fmt.Errorf("payment: payout schedule timezone %q: %w", r.cfg.Timezone, err)
	}
	c := cron.New(cron.WithLocation(loc))
	if _, err := c.AddFunc(r.cfg.Schedule, func() {
		summary, err := r.Run(context.WithoutCancel(ctx))
		switch {
		case errors.Is(err, ErrPayoutRunInProgress):
			// The expected outcome in every replica but one. It is not a
			// failure and must not be logged as one, or the nightly error
			// budget is spent on the design working.
			r.log.Debug().Msg("payout run skipped; another replica holds the lease")
			return
		case err != nil:
			r.log.Error().Err(err).Msg("scheduled payout run failed")
			return
		}
		r.log.Info().
			Int("paid", summary.Paid).Int("created", summary.Created).
			Int("resumed", summary.Resumed).Int("failed", summary.Failed).
			Int64("amount_cents", summary.AmountCents).
			Msg("scheduled payout run complete")
	}); err != nil {
		return fmt.Errorf("payment: payout cron expression %q: %w", r.cfg.Schedule, err)
	}

	c.Start()
	r.log.Info().Str("schedule", r.cfg.Schedule).Str("tz", r.cfg.Timezone).Msg("payout job scheduled")
	<-ctx.Done()
	<-c.Stop().Done()
	r.log.Info().Msg("payout job stopped")
	return nil
}

// Run executes one settlement pass.
//
// When a lease is configured, Run holds it for the duration. A replica that
// cannot acquire it returns ErrPayoutRunInProgress and does nothing, which is
// the correct answer both for the 02:00 cron firing in every pod and for an
// operator pressing POST /payouts/run while that cron is still working.
func (r *PayoutRunner) Run(ctx context.Context) (RunSummary, error) {
	if r.lease != nil {
		token, acquired, err := r.lease.Lock(ctx, PayoutLockKey, r.cfg.LeaseTTL)
		switch {
		case err != nil:
			// Fail OPEN, deliberately, and unlike most of this file. The
			// database guarantees are what stop a doctor being paid twice; the
			// lease only stops the pods tripping over each other. Refusing to
			// settle anyone because Redis is down would turn a cache outage
			// into doctors not being paid, which is the worse failure.
			r.log.Error().Err(err).Msg("payout run lease unavailable; proceeding without it")
		case !acquired:
			return RunSummary{StartedAt: r.now().UTC()}, ErrPayoutRunInProgress
		default:
			defer func() {
				if err := r.lease.Unlock(context.WithoutCancel(ctx), PayoutLockKey, token); err != nil {
					r.log.Warn().Err(err).Msg("payout run lease could not be released")
				}
			}()
		}
	}

	now := r.now().UTC()
	cutoff := now.Add(-r.cfg.HoldPeriod)
	periodEnd := cutoff.Truncate(24 * time.Hour)
	periodStart := periodEnd.AddDate(0, 0, -1)

	summary := RunSummary{StartedAt: now, PeriodStart: periodStart, PeriodEnd: periodEnd}

	// Step 1: create tonight's batches. Doing this before the settlement sweep
	// means a batch created now is settled in the same run.
	dues, err := r.store.DoctorsWithDuePayments(ctx, cutoff, r.cfg.MaxDoctorsPerRun)
	if err != nil {
		return summary, err
	}
	for _, due := range dues {
		created, err := r.prepareBatch(ctx, due, periodStart, periodEnd, cutoff)
		switch {
		case err != nil:
			r.log.Error().Err(err).Str("doctor_id", logger.MaskID(due.DoctorID.String())).
				Msg("could not prepare payout batch")
			summary.Failed++
		case created:
			summary.Created++
		default:
			summary.Skipped++
		}
	}

	// Step 2: settle everything unsettled, including batches abandoned by an
	// earlier crashed run. This is the resume.
	ids, err := r.store.UnsettledPayoutIDs(ctx, r.cfg.MaxDoctorsPerRun*2)
	if err != nil {
		return summary, err
	}
	for _, id := range ids {
		paid, amount, err := r.settle(ctx, id)
		switch {
		case err != nil:
			r.log.Error().Err(err).Str("payout_id", logger.MaskID(id.String())).Msg("payout settlement failed")
			summary.Failed++
		case paid:
			summary.Paid++
			summary.AmountCents += amount
		default:
			summary.Resumed++
		}
	}

	summary.FinishedAt = r.now().UTC()
	return summary, nil
}

// prepareBatch creates (or finds) the payout row for one doctor and claims the
// payments into it. It reports whether it created a new batch.
func (r *PayoutRunner) prepareBatch(ctx context.Context, due DoctorDue, periodStart, periodEnd, cutoff time.Time) (bool, error) {
	created := false
	currency := due.Currency
	if currency == "" {
		currency = CurrencyLKR
	}

	err := r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		existing, err := tx.FindPayoutByPeriod(ctx, due.DoctorID, periodStart, periodEnd)
		switch {
		case err == nil:
			// Only a PENDING batch may grow.
			//
			// The old test here was `!= PayoutPaid`, which let a batch be
			// enlarged while its transfer was in flight. The cron scheduler
			// runs in every replica and POST /payouts/run can fire on top, so
			// this is a race two pods reach at 02:00, not a theoretical one:
			//
			//	replica A  settle() phase 1 -- row marked `processing`,
			//	           transfer of X cents in flight
			//	replica B  prepareBatch     -- claims newly-eligible payments
			//	           into that same row, amount now X+Y
			//	replica A  settle() phase 3 -- re-reads the row and writes
			//	           ledger legs for X+Y against a transfer of X
			//
			// The Y cents are stamped with a payout_id, so they are never
			// claimed again and the doctor is simply underpaid; and the
			// discrepancy lands in ledger_entries, which is append-only and
			// therefore cannot be corrected in place -- only annotated by a
			// second, compensating entry that no code writes.
			//
			// `processing`, `failed` and `cancelled` are all refused for the
			// same reason: none of them will transfer more money on account of
			// what we add now. The payments stay unclaimed and are picked up
			// by the next period, which is exactly what the `paid` branch
			// already did.
			if existing.Status != PayoutPending {
				return nil
			}
			// A pending batch from a crashed run. Claim anything it missed and
			// let the settlement sweep finish it.
			count, total, err := tx.ClaimPaymentsForPayout(ctx, existing.ID, due.DoctorID, cutoff)
			if err != nil {
				return err
			}
			if count == 0 {
				return nil
			}
			existing.PaymentCount += count
			existing.AmountCents += total
			return tx.UpdatePayout(ctx, &existing)

		case errors.Is(err, ErrNotFound):
			p := Payout{
				ID:          uuid.New(),
				DoctorID:    due.DoctorID,
				PeriodStart: periodStart,
				PeriodEnd:   periodEnd,
				Currency:    currency,
				Status:      PayoutPending,
				Provider:    r.cfg.Provider,
				// Stable for the life of the row: every retry of the transfer
				// presents this same key to the rail.
				IdempotencyKey: "payout:" + uuid.NewString(),
			}
			if err := tx.InsertPayout(ctx, &p); err != nil {
				return err
			}
			count, total, err := tx.ClaimPaymentsForPayout(ctx, p.ID, due.DoctorID, cutoff)
			if err != nil {
				return err
			}
			if count == 0 {
				// Another run beat us to the payments. Cancel the empty batch
				// rather than leaving a zero-value transfer to be attempted.
				p.Status = PayoutCancelled
				p.FailureReason = "no unclaimed payments remained"
				return tx.UpdatePayout(ctx, &p)
			}
			p.PaymentCount = count
			p.AmountCents = total
			if err := tx.UpdatePayout(ctx, &p); err != nil {
				return err
			}
			created = true
			return nil

		default:
			return err
		}
	})
	if errors.Is(err, ErrDuplicate) {
		// Two replicas ran the job at the same instant; the loser does nothing
		// and the winner's batch is settled by the sweep below.
		return false, nil
	}
	return created, err
}

// settle performs the provider transfer for one payout batch and records the
// outcome. It reports whether money actually moved on this call.
func (r *PayoutRunner) settle(ctx context.Context, payoutID uuid.UUID) (paid bool, amountCents int64, err error) {
	// Phase 1: take the batch, mark it in flight.
	var batch Payout
	err = r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		p, err := tx.LockPayout(ctx, payoutID)
		if err != nil {
			return err
		}
		if p.Status == PayoutPaid || p.Status == PayoutCancelled {
			batch = p
			return nil
		}
		if p.AmountCents <= 0 {
			p.Status = PayoutCancelled
			p.FailureReason = "nothing to settle"
			if _, err := tx.ReleasePaymentsFromPayout(ctx, p.ID); err != nil {
				return err
			}
			batch = p
			return tx.UpdatePayout(ctx, &p)
		}
		now := r.now().UTC()
		p.Status = PayoutProcessing
		p.InitiatedAt = &now
		p.Attempts++
		if err := tx.UpdatePayout(ctx, &p); err != nil {
			return err
		}
		batch = p
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	if batch.Status == PayoutPaid || batch.Status == PayoutCancelled {
		return false, 0, nil
	}

	// Phase 2: the transfer itself, outside any transaction.
	rail, err := r.providers.Get(batch.Provider)
	if err != nil {
		return false, 0, err
	}
	account, destErr := r.destinations.PayoutAccount(ctx, batch.DoctorID)

	var res PayoutResult
	var transferErr error
	if destErr != nil {
		transferErr = destErr
	} else {
		res, transferErr = rail.Payout(ctx, PayoutRequest{
			PayoutID:           batch.ID,
			DoctorID:           batch.DoctorID,
			DestinationAccount: account,
			AmountCents:        batch.AmountCents,
			Currency:           batch.Currency,
			Description:        fmt.Sprintf("Telemed settlement %s to %s", batch.PeriodStart.Format(time.DateOnly), batch.PeriodEnd.Format(time.DateOnly)),
			IdempotencyKey:     batch.IdempotencyKey,
		})
	}

	// Phase 3: record the outcome, move the ledger, announce it.
	err = r.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		p, err := tx.LockPayout(ctx, batch.ID)
		if err != nil {
			return err
		}
		if p.Status == PayoutPaid {
			return nil // a concurrent run already recorded it
		}

		// The failure branch deliberately comes before the in-flight-mutation
		// assertion below: nothing was transferred, so returning the row to
		// pending or failed is safe whatever its amount now is, and the next
		// run re-reads it from scratch.
		if transferErr != nil {
			// Retryable and terminal failures get different states, and the
			// difference is what makes the resume work. "Provider unavailable"
			// goes back to pending so the next run picks it up automatically;
			// a rejection or a missing account goes to failed, because
			// retrying it nightly forever would bury the one payout an
			// operator actually needs to look at.
			if errors.Is(transferErr, ErrProviderUnavailable) {
				p.Status = PayoutPending
			} else {
				p.Status = PayoutFailed
			}
			p.FailureReason = classifyPayoutFailure(transferErr)
			return tx.UpdatePayout(ctx, &p)
		}

		// The money that actually left the building is the amount phase 1
		// read and phase 2 sent -- not whatever the row says now. If those
		// have drifted, the ledger would record a settlement that did not
		// happen, so stop before writing anything.
		if p.AmountCents != batch.AmountCents || p.PaymentCount != batch.PaymentCount {
			return fmt.Errorf("%w: %s was %d cents over %d payments when the transfer was made and is now %d over %d",
				ErrPayoutMutatedInFlight, p.ID, batch.AmountCents, batch.PaymentCount, p.AmountCents, p.PaymentCount)
		}

		now := r.now().UTC()
		p.Status = PayoutPaid
		p.TransferID = res.TransferID
		p.PaidAt = &now
		p.FailureReason = ""
		if err := tx.UpdatePayout(ctx, &p); err != nil {
			return err
		}

		// DEBIT liability:doctor_payable / CREDIT asset:cash. The doctor is no
		// longer owed the money because it has left our account.
		//
		// Both legs are written from `batch` -- the phase 1 snapshot the
		// transfer was made against -- rather than from the row just re-read,
		// so the ledger states the amount that moved even if the two ever
		// diverge. The guard above means they cannot; writing it this way
		// means the ledger does not depend on that guard being correct.
		did := p.DoctorID
		poid := p.ID
		if err := tx.WriteLedger(ctx, LedgerTransaction{
			EntryID:   uuid.New(),
			EntryType: EntryPayoutSent,
			Legs: []LedgerEntry{
				{
					Account: AccountDoctorPayable, Direction: Debit, AmountCents: batch.AmountCents,
					Currency: batch.Currency, PayoutID: &poid, DoctorID: &did, OccurredAt: now,
					Description: fmt.Sprintf("settlement of %d consultations", batch.PaymentCount),
				},
				{
					Account: AccountCash, Direction: Credit, AmountCents: batch.AmountCents,
					Currency: batch.Currency, PayoutID: &poid, DoctorID: &did, OccurredAt: now,
					Description: "funds transferred via " + string(batch.Provider),
				},
			},
		}); err != nil {
			return err
		}

		paid = true
		batch = p
		return tx.Enqueue(ctx, events.SubjectPayoutSent, p.ID.String(), PayoutEvent{
			PayoutID: p.ID, DoctorID: p.DoctorID, AmountCents: p.AmountCents,
			Currency: p.Currency, PaymentCount: p.PaymentCount,
			PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd,
			Provider: string(p.Provider), TransferID: p.TransferID, OccurredAt: now,
		})
	})
	if err != nil {
		return false, 0, err
	}
	if transferErr != nil {
		return false, 0, transferErr
	}
	return paid, batch.AmountCents, nil
}

// classifyPayoutFailure turns a provider error into a short, operator-readable
// reason. It never embeds the raw provider message, which can contain an
// account identifier.
func classifyPayoutFailure(err error) string {
	switch {
	case errors.Is(err, ErrNoDestination):
		return "no payout account on file for this doctor"
	case errors.Is(err, ErrProviderUnavailable):
		return "provider unavailable; will retry on the next run"
	case errors.Is(err, ErrProviderRejected):
		return "provider rejected the transfer"
	case errors.Is(err, ErrUnsupported):
		return "configured payout rail does not support transfers"
	default:
		return "transfer failed"
	}
}
