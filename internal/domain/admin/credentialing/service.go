package credentialing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

// DocumentStore is the one object-storage operation this domain needs.
//
// Declared here rather than taken as platform/storage.Storage because the
// credentialing service is a consumer only -- it turns object keys that
// doctor-service recorded into short-lived presigned URLs, and it must never
// be able to Put, Delete or Get the bytes of a scanned NIC. Narrowing the
// interface at the point of use is what enforces that, and it keeps the
// package's test fake to a single method.
type DocumentStore interface {
	PresignedGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
}

// Service implements the credentialing business rules: presigning document
// URLs, checklist item updates, and the approve/reject decision that
// publishes doctor.approved/doctor.rejected via the outbox.
type Service struct {
	pool       database.Pool
	repo       *Repository
	storage    DocumentStore
	outbox     *events.Outbox
	bucket     string
	presignTTL time.Duration
	apps       ApplicationVerifier
	pending    PendingApplicationSource
}

// Presign TTL bounds. A presigned URL is a bearer credential for a scanned NIC
// or SLMC certificate: anyone holding the link has the document until it
// lapses, and links leak -- into browser history, into a screenshot, into a
// support ticket. So the TTL is a data-exposure setting, not a convenience
// knob, and it is bounded here rather than trusted from config.
//
// The default is five minutes: long enough to open a document from the queue,
// short enough that a leaked link is stale before it travels. The ceiling is
// fifteen. A reviewer who leaves the page open past the TTL re-fetches the row
// -- DocumentsExpireAt on the response is what tells the console to.
const (
	DefaultPresignTTL = 5 * time.Minute
	MaxPresignTTL     = 15 * time.Minute
)

func NewService(pool database.Pool, repo *Repository, store DocumentStore, outbox *events.Outbox, bucket string, presignTTL time.Duration) *Service {
	if presignTTL <= 0 {
		presignTTL = DefaultPresignTTL
	}
	if presignTTL > MaxPresignTTL {
		presignTTL = MaxPresignTTL
	}
	return &Service{pool: pool, repo: repo, storage: store, outbox: outbox, bucket: bucket, presignTTL: presignTTL}
}

// SetApplicationVerifier wires doctor-service SoR approve/reject for public applications.
func (s *Service) SetApplicationVerifier(v ApplicationVerifier) {
	s.apps = v
}

// SetPendingApplicationSource wires the doctor-service pending list used to
// heal the verification queue when application_submitted events were lost.
func (s *Service) SetPendingApplicationSource(src PendingApplicationSource) {
	s.pending = src
}

func (s *Service) ListPending(ctx context.Context, page, perPage int) ([]PresignedDoctorSummary, int64, error) {
	s.syncPendingApplications(ctx)
	docs, total, err := s.repo.ListPending(ctx, page, perPage)
	if err != nil {
		return nil, 0, err
	}
	out := make([]PresignedDoctorSummary, len(docs))
	for i := range docs {
		out[i] = s.presign(ctx, docs[i])
	}
	return out, total, nil
}

// syncPendingApplications projects any doctor-service pending applications that
// are missing from doctor_projection so reviewers can still open and decide them.
func (s *Service) syncPendingApplications(ctx context.Context) {
	if s.pending == nil {
		return
	}
	apps, err := s.pending.ListPendingApplications(ctx)
	if err != nil {
		log := logger.FromContext(ctx)
		log.Warn().Err(err).Msg("credentialing: sync pending applications failed")
		return
	}
	for _, app := range apps {
		if _, err := s.repo.GetDoctor(ctx, app.ID); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			log := logger.FromContext(ctx)
			log.Warn().Err(err).
				Str("application_id", app.ID.String()).
				Msg("credentialing: lookup during pending sync failed")
			continue
		}
		summary := DoctorSummary{
			DoctorID:        app.ID,
			FullName:        app.FullName,
			Email:           app.Email,
			Phone:           app.Phone,
			SLMCNumber:      app.SLMCNumber,
			YearsExperience: app.YearsExperience,
			SpecialtyCode:   app.Specialty,
			FeeCents:        app.FeeCents,
			RegisteredAt:    app.CreatedAt,
		}
		if summary.RegisteredAt.IsZero() {
			summary.RegisteredAt = time.Now().UTC()
		}
		if err := s.repo.UpsertDoctorFromEvent(ctx, uuid.New(), summary); err != nil {
			log := logger.FromContext(ctx)
			log.Warn().Err(err).
				Str("application_id", app.ID.String()).
				Msg("credentialing: could not project pending application into queue")
		}
	}
}

// GetDoctor returns the projected summary with presigned document URLs and
// the most recent checklist, if one has ever been started. A doctor with no
// checklist yet returns a zero-value Checklist -- the handler renders that
// as "not started", not an error.
func (s *Service) GetDoctor(ctx context.Context, id uuid.UUID) (PresignedDoctorSummary, Checklist, error) {
	d, err := s.repo.GetDoctor(ctx, id)
	if err != nil {
		return PresignedDoctorSummary{}, Checklist{}, err
	}
	checklist, err := s.repo.GetLatestChecklist(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return PresignedDoctorSummary{}, Checklist{}, err
	}
	return s.presign(ctx, d), checklist, nil
}

// UpdateChecklist applies a partial checklist update, creating the pending
// checklist row on first touch.
func (s *Service) UpdateChecklist(ctx context.Context, doctorID uuid.UUID, u ChecklistUpdate) (Checklist, error) {
	if _, err := s.repo.GetDoctor(ctx, doctorID); err != nil {
		return Checklist{}, err
	}
	current, err := s.repo.GetOrCreatePendingChecklist(ctx, doctorID)
	if err != nil {
		return Checklist{}, err
	}

	before := current
	after, err := s.repo.UpdateChecklistItems(ctx, current.ID, u)
	if err != nil {
		return Checklist{}, err
	}

	audit.Stage(ctx, audit.Draft{
		Action:       "doctor.checklist_updated",
		ResourceType: "doctor",
		ResourceID:   doctorID.String(),
		OldValue:     checklistSnapshot(before),
		NewValue:     checklistSnapshot(after),
	})
	return after, nil
}

// Verify records the approve/reject decision, updates the local projection,
// and publishes doctor.approved/doctor.rejected via the outbox for legacy
// doctor.registered rows. Public applications are decided in doctor-service
// (SoR); this service then updates its checklist/projection without emitting
// doctor.approved (that fires later when OTP attach creates the doctors row).
func (s *Service) Verify(ctx context.Context, doctorID uuid.UUID, d VerifyDecision) (Checklist, error) {
	if _, err := s.repo.GetDoctor(ctx, doctorID); err != nil {
		return Checklist{}, err
	}

	viaApplication := false
	if s.apps != nil {
		err := s.apps.VerifyApplication(ctx, doctorID, d.Approve, d.Reason)
		switch {
		case err == nil:
			viaApplication = true
		case errors.Is(err, ErrApplicationNotFound):
			// Legacy credentialing row without a public application.
		default:
			return Checklist{}, err
		}
	}

	// Public applications land in the queue with no checklist row yet; create
	// one so Decide can record the decision the same way as legacy register.
	if _, err := s.repo.GetOrCreatePendingChecklist(ctx, doctorID); err != nil {
		return Checklist{}, err
	}

	var result Checklist
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		c, err := s.repo.Decide(ctx, tx, doctorID, d)
		if err != nil {
			return err
		}
		result = c
		if viaApplication {
			return nil
		}
		subject := events.SubjectDoctorRejected
		if d.Approve {
			subject = events.SubjectDoctorApproved
		}
		payload := map[string]any{
			"doctor_id":  doctorID,
			"reason":     d.Reason,
			"decided_by": d.DeciderID,
		}
		return s.outbox.Enqueue(ctx, tx, subject, doctorID.String(), payload)
	})
	if err != nil {
		return Checklist{}, err
	}

	action := "doctor.rejected"
	if d.Approve {
		action = "doctor.approved"
	}
	audit.Stage(ctx, audit.Draft{
		Action:       action,
		ResourceType: "doctor",
		ResourceID:   doctorID.String(),
		NewValue:     map[string]any{"reason": d.Reason, "overall_status": result.OverallStatus},
	})
	return result, nil
}

func (s *Service) presign(ctx context.Context, d DoctorSummary) PresignedDoctorSummary {
	out := PresignedDoctorSummary{DoctorSummary: d}
	log := logger.FromContext(ctx)

	presign := func(key string) string {
		if key == "" || s.storage == nil {
			return ""
		}
		url, err := s.storage.PresignedGet(ctx, s.bucket, key, s.presignTTL)
		if err != nil {
			// The key is still returned on the row, so the failure is visible
			// as "document present, URL missing" rather than as an application
			// that appears to have submitted nothing.
			log.Warn().Err(err).Str("key", key).Str("bucket", s.bucket).
				Msg("credentialing: failed to presign document URL")
			return ""
		}
		return url
	}

	out.SLMCCertificateURL = presign(d.SLMCCertificateKey)
	out.NICDocumentURL = presign(d.NICDocumentKey)
	out.DegreeCertificateURL = presign(d.DegreeCertificateKey)
	out.PhotoURL = presign(d.PhotoKey)
	if !d.Empty() {
		out.DocumentsExpireAt = time.Now().UTC().Add(s.presignTTL)
	}
	return out
}

func checklistSnapshot(c Checklist) map[string]any {
	return map[string]any{
		"slmc_format_valid":     c.SLMCFormatValid,
		"slmc_registry_checked": c.SLMCRegistryChecked,
		"experience_verified":   c.ExperienceVerified,
		"nic_matches":           c.NICMatches,
		"photo_clear":           c.PhotoClear,
		"overall_status":        c.OverallStatus,
	}
}
