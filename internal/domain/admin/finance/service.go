package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/sysconfig"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

const commissionRulesKey = "commission_rules"

// Service implements the ledger view, commission rule editing on top of
// sysconfig, and the two money-moving commands published over the outbox.
type Service struct {
	pool    database.Pool
	repo    *Repository
	outbox  *events.Outbox
	configs *sysconfig.Service
}

func NewService(pool database.Pool, repo *Repository, outbox *events.Outbox, configs *sysconfig.Service) *Service {
	return &Service{pool: pool, repo: repo, outbox: outbox, configs: configs}
}

func (s *Service) Ledger(ctx context.Context, f LedgerFilter) ([]LedgerEntry, int64, error) {
	return s.repo.List(ctx, f)
}

// Export bounds. The audit subsystem already solved this problem
// (ErrExportUnbounded / ErrExportWindowTooWide / ErrExportTooLarge); the
// finance export got none of it, so a bare GET .../payments.csv was a
// full-table sort-and-stream of every payment on the platform, patient_id and
// doctor_id included.
const (
	// MaxExportWindow is the widest from/to a single export may cover. A
	// quarter is the longest window a finance reconciliation legitimately
	// needs in one file.
	MaxExportWindow = 92 * 24 * time.Hour
	// MaxExportRows bounds one file. Beyond this, narrow the window.
	MaxExportRows = 100_000
)

var (
	ErrExportUnbounded = errors.New(
		"finance: export requires both from and to (YYYY-MM-DD); an unbounded export is not permitted")
	ErrExportWindowTooWide = fmt.Errorf(
		"finance: export window must not exceed %d days; narrow from/to and export in several calls",
		int(MaxExportWindow.Hours()/24))
	ErrExportTooLarge = fmt.Errorf(
		"finance: export matches more than %d rows; narrow from/to or filter by doctor or status",
		MaxExportRows)
)

// PrepareExport validates that a filter is bounded enough to export and
// returns how many rows it matches.
//
// Separate from ExportLedger for the same reason audit.PrepareExport is: once
// a 200 and a CSV header are on the wire there is no way to turn the response
// into an error, so both the bound and the audit row have to be settled first.
func (s *Service) PrepareExport(ctx context.Context, f LedgerFilter) (int64, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return 0, ErrExportUnbounded
	}
	if f.To.Sub(f.From) > MaxExportWindow {
		return 0, ErrExportWindowTooWide
	}
	count, err := s.repo.CountMatching(ctx, f)
	if err != nil {
		return 0, err
	}
	if count > MaxExportRows {
		return 0, ErrExportTooLarge
	}
	return count, nil
}

func (s *Service) ExportLedger(ctx context.Context, f LedgerFilter, emit func(LedgerEntry) error) error {
	return s.repo.StreamExport(ctx, f, emit)
}

// CurrentCommissionRule returns the currently-effective commission_rules
// config, or a zero-value rule (0% default) if none has ever been set.
func (s *Service) CurrentCommissionRule(ctx context.Context) (CommissionRule, int, error) {
	cfg, err := s.configs.Get(ctx, commissionRulesKey)
	if err != nil {
		if errors.Is(err, sysconfig.ErrNotFound) {
			return CommissionRule{}, 0, nil
		}
		return CommissionRule{}, 0, err
	}
	rule, err := DecodeCommissionValue(cfg.Value)
	if err != nil {
		return CommissionRule{}, 0, err
	}
	return rule, cfg.Version, nil
}

// CurrentCommissionConfig is the same read with the versioned row, so the
// console can render SystemConfig metadata (who, when) next to the editor.
func (s *Service) CurrentCommissionConfig(ctx context.Context) (sysconfig.Config, CommissionRule, error) {
	cfg, err := s.configs.Get(ctx, commissionRulesKey)
	if err != nil {
		if errors.Is(err, sysconfig.ErrNotFound) {
			return sysconfig.Config{Key: commissionRulesKey}, CommissionRule{}, nil
		}
		return sysconfig.Config{}, CommissionRule{}, err
	}
	rule, err := DecodeCommissionValue(cfg.Value)
	if err != nil {
		return sysconfig.Config{}, CommissionRule{}, err
	}
	return cfg, rule, nil
}

// SetCommissionRule creates a new commission_rules version (sysconfig never
// mutates a previous version in place -- see internal/sysconfig).
func (s *Service) SetCommissionRule(ctx context.Context, caller middleware.Principal, actorID uuid.UUID, rule CommissionRule, effectiveFrom time.Time) (sysconfig.Config, error) {
	value, err := json.Marshal(rule)
	if err != nil {
		return sysconfig.Config{}, err
	}
	// The principal goes through so sysconfig applies its per-key write
	// policy here too. This route is already gated to finance/super_admin by
	// GroupFinance; passing the caller means the rule is enforced by the layer
	// that owns the row rather than by whichever router happened to be used.
	return s.configs.Put(ctx, caller, actorID, commissionRulesKey, value, effectiveFrom)
}

// TriggerPayoutBatch publishes admin.payout_batch_requested for the given
// window. payment-service is expected to compute and execute the batch and
// publish payout.sent per doctor.
func (s *Service) TriggerPayoutBatch(ctx context.Context, adminID uuid.UUID, from, to time.Time) error {
	if err := s.repo.UpsertPayoutBatchRequest(ctx, from, to); err != nil {
		return err
	}
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.outbox.Enqueue(ctx, tx, events.SubjectAdminPayoutBatchRequested, "", events.AdminPayoutBatchRequested{
			From: from, To: to, AdminID: adminID,
		})
	})
	if err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "finance.payout_batch_requested", ResourceType: "payout_batch", ResourceID: from.Format("2006-01-02") + "_" + to.Format("2006-01-02"),
		NewValue: map[string]any{"from": from, "to": to},
	})
	return nil
}

// ApproveRefund publishes admin.refund_approved for a specific payment.
// payment-service is expected to execute the refund with its provider and
// publish payment.refunded.
func (s *Service) ApproveRefund(ctx context.Context, adminID, paymentID uuid.UUID, amountCents int64, reason string) error {
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.outbox.Enqueue(ctx, tx, events.SubjectAdminRefundApproved, paymentID.String(), events.AdminRefundApproved{
			PaymentID: paymentID, AmountCents: amountCents, Reason: reason, AdminID: adminID,
		})
	})
	if err != nil {
		return err
	}
	audit.Stage(ctx, audit.Draft{
		Action: "finance.refund_approved", ResourceType: "payment", ResourceID: paymentID.String(),
		NewValue: map[string]any{"amount_cents": amountCents, "reason": reason},
	})
	return nil
}

func (s *Service) ListRefunds(ctx context.Context, status string, page, perPage int) ([]RefundRequest, int64, error) {
	return s.repo.ListRefunds(ctx, status, page, perPage)
}

func (s *Service) ListPayoutBatches(ctx context.Context, page, perPage int) ([]PayoutBatch, int64, error) {
	return s.repo.ListPayoutBatches(ctx, page, perPage)
}

func (s *Service) CommissionHistory(ctx context.Context) ([]sysconfig.Config, error) {
	return s.configs.History(ctx, commissionRulesKey)
}

// OpenFromDispute records a pending refund for the payments console when a
// dispute asks for money back. Missing payment (consult unpaid) is a no-op:
// there is nothing to refund yet.
func (s *Service) OpenFromDispute(ctx context.Context, disputeID, appointmentID uuid.UUID, amountCents *int64, reason string) error {
	pay, err := s.repo.PaymentByAppointment(ctx, appointmentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	cents := pay.AmountCents
	if amountCents != nil && *amountCents > 0 {
		cents = *amountCents
	}
	if cents <= 0 {
		return nil
	}
	_, err = s.repo.InsertRefund(ctx, RefundRequest{
		PaymentID: pay.PaymentID, AppointmentID: appointmentID, DisputeID: disputeID,
		AmountCents: cents, Currency: pay.Currency, Reason: reason,
	})
	return err
}

func (s *Service) DecideRefund(ctx context.Context, actorID, refundID uuid.UUID, decision, note string) error {
	req, err := s.repo.GetRefund(ctx, refundID)
	if err != nil {
		return err
	}
	if req.Status != "pending" {
		return fmt.Errorf("finance: refund %s is already %s", refundID, req.Status)
	}
	switch decision {
	case "approved":
		if err := s.ApproveRefund(ctx, actorID, req.PaymentID, req.AmountCents, note); err != nil {
			return err
		}
		_, err = s.repo.DecideRefund(ctx, refundID, actorID, "approved")
		return err
	case "rejected":
		if _, err := s.repo.DecideRefund(ctx, refundID, actorID, "rejected"); err != nil {
			return err
		}
		audit.Stage(ctx, audit.Draft{
			Action: "finance.refund_rejected", ResourceType: "payment", ResourceID: req.PaymentID.String(),
			NewValue: map[string]any{"reason": note},
		})
		return nil
	default:
		return fmt.Errorf("finance: decision must be approved or rejected")
	}
}
