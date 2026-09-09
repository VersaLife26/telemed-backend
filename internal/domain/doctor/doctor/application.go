package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
)

// ApplicationStatus is the public-apply lifecycle before (and until) OTP attach.
type ApplicationStatus string

const (
	ApplicationPending   ApplicationStatus = "pending"
	ApplicationApproved  ApplicationStatus = "approved"
	ApplicationRejected  ApplicationStatus = "rejected"
	ApplicationActivated ApplicationStatus = "activated"
)

// Application is a doctor registration collected before any OTP/user account.
type Application struct {
	ID              uuid.UUID
	Phone           string
	Email           string
	DisplayName     string
	SLMCNumber      string
	Specialty       string
	Languages       []Language
	ExperienceYears int
	FeeCents        int64
	Bio             string
	Status          ApplicationStatus
	RejectionReason string
	DecidedAt       *time.Time
	DecidedBy       *uuid.UUID
	ActivatedUserID *uuid.UUID
	ActivatedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ApplyInput is the validated public apply payload.
type ApplyInput struct {
	Phone           string
	Email           string
	DisplayName     string
	SLMCNumber      string
	Specialty       string
	Languages       []Language
	ExperienceYears int
	FeeCents        int64
	Bio             string
}

var (
	ErrApplicationExists   = errors.New("doctor: an open application already exists for this phone or SLMC number")
	ErrApplicationNotFound = errors.New("doctor: application not found")
	ErrApplicationNotReady = errors.New("doctor: application is not approved for activation")
)

// Apply creates a pending doctor application and publishes doctor.application_submitted.
func (s *Service) Apply(ctx context.Context, in ApplyInput) (Application, error) {
	phone := httpx.NormalizePhone(in.Phone)
	if phone == "" {
		return Application{}, fmt.Errorf("%w: invalid phone", ErrInvalidTransition)
	}
	email := strings.TrimSpace(strings.ToLower(in.Email))
	if email == "" {
		return Application{}, fmt.Errorf("%w: email required", ErrInvalidTransition)
	}
	if ok, err := s.repo.SpecialtyExists(ctx, in.Specialty); err != nil {
		return Application{}, err
	} else if !ok {
		return Application{}, fmt.Errorf("%w: unknown specialty %q", ErrInvalidTransition, in.Specialty)
	}
	if len(in.Languages) == 0 {
		return Application{}, fmt.Errorf("%w: at least one language is required", ErrInvalidTransition)
	}

	app := Application{
		ID:              uuid.New(),
		Phone:           phone,
		Email:           email,
		DisplayName:     strings.TrimSpace(in.DisplayName),
		SLMCNumber:      strings.TrimSpace(in.SLMCNumber),
		Specialty:       in.Specialty,
		Languages:       in.Languages,
		ExperienceYears: in.ExperienceYears,
		FeeCents:        in.FeeCents,
		Bio:             strings.TrimSpace(in.Bio),
		Status:          ApplicationPending,
	}

	now := time.Now().UTC()
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.CreateApplication(ctx, tx, &app); err != nil {
			return err
		}
		payload := events.DoctorApplicationSubmitted{
			ApplicationID:   app.ID,
			FullName:        app.DisplayName,
			Email:           app.Email,
			Phone:           app.Phone,
			SLMCNumber:      app.SLMCNumber,
			Specialty:       app.Specialty,
			YearsExperience: app.ExperienceYears,
			FeeCents:        app.FeeCents,
			Languages:       languagesToStrings(app.Languages),
			CreatedAt:       now,
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorApplicationSubmitted, app.ID.String(), payload)
	})
	if err != nil {
		return Application{}, err
	}
	app.CreatedAt = now
	app.UpdatedAt = now
	return app, nil
}

// ApplicationEligibility is the doctor-portal OTP gate.
type ApplicationEligibility struct {
	Status        string `json:"status"` // none | pending | approved | rejected | activated
	ApplicationID string `json:"application_id,omitempty"`
	Message       string `json:"message"`
}

// EligibilityByPhone reports whether the doctor portal may send OTP for phone.
func (s *Service) EligibilityByPhone(ctx context.Context, rawPhone string) (ApplicationEligibility, error) {
	phone := httpx.NormalizePhone(rawPhone)
	if phone == "" {
		return ApplicationEligibility{}, fmt.Errorf("%w: invalid phone", ErrInvalidTransition)
	}
	app, err := s.repo.FindOpenOrRecentApplicationByPhone(ctx, phone)
	if errors.Is(err, ErrApplicationNotFound) {
		return ApplicationEligibility{
			Status:  "none",
			Message: "Register as a doctor first. After approval you can verify with OTP.",
		}, nil
	}
	if err != nil {
		return ApplicationEligibility{}, err
	}
	out := ApplicationEligibility{ApplicationID: app.ID.String()}
	switch app.Status {
	case ApplicationPending:
		out.Status = "pending"
		out.Message = "Your application is under review. You will receive an email when it is approved."
	case ApplicationApproved:
		out.Status = "approved"
		out.Message = "Your application is approved. Enter the OTP sent to your phone to activate your account."
	case ApplicationRejected:
		out.Status = "rejected"
		out.Message = "Your application was not approved. Contact support if you need to re-apply."
	case ApplicationActivated:
		out.Status = "activated"
		out.Message = "Your doctor account is active. You can sign in with OTP."
	default:
		out.Status = string(app.Status)
		out.Message = "Unknown application status."
	}
	return out, nil
}

// GetApplicationByPhone returns the open (pending/approved) application for a phone.
func (s *Service) GetApplicationByPhone(ctx context.Context, rawPhone string) (Application, error) {
	phone := httpx.NormalizePhone(rawPhone)
	if phone == "" {
		return Application{}, ErrApplicationNotFound
	}
	return s.repo.FindOpenApplicationByPhone(ctx, phone)
}

// VerifyApplication is the admin approve/reject decision on a public application.
func (s *Service) VerifyApplication(ctx context.Context, id uuid.UUID, approve bool, reason string, actorID *uuid.UUID) (Application, error) {
	app, err := s.repo.GetApplication(ctx, id)
	if err != nil {
		return Application{}, err
	}
	if approve && app.Status == ApplicationApproved {
		return app, nil
	}
	if !approve && app.Status == ApplicationRejected {
		return app, nil
	}
	if app.Status != ApplicationPending {
		return Application{}, fmt.Errorf("%w: application is %s", ErrInvalidTransition, app.Status)
	}
	if !approve && strings.TrimSpace(reason) == "" {
		return Application{}, ErrReasonRequired
	}

	now := time.Now().UTC()
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		status := ApplicationApproved
		subject := events.SubjectDoctorApplicationApproved
		var payload any
		if approve {
			payload = events.DoctorApplicationApproved{
				ApplicationID: app.ID,
				FullName:      app.DisplayName,
				Email:         app.Email,
				Phone:         app.Phone,
				Specialty:     app.Specialty,
				ApprovedAt:    now,
			}
		} else {
			status = ApplicationRejected
			subject = events.SubjectDoctorApplicationRejected
			payload = events.DoctorApplicationRejected{
				ApplicationID: app.ID,
				FullName:      app.DisplayName,
				Email:         app.Email,
				Phone:         app.Phone,
				Reason:        reason,
				RejectedAt:    now,
			}
		}
		if err := s.repo.DecideApplication(ctx, tx, id, status, reason, actorID, now); err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, subject, id.String(), payload)
	})
	if err != nil {
		return Application{}, err
	}
	return s.repo.GetApplication(ctx, id)
}

// AttachInput binds an approved application to a newly verified user account.
type AttachInput struct {
	ApplicationID uuid.UUID
	UserID        uuid.UUID
	Email         string
}

// Attach creates the doctors row from an approved application and publishes doctor.approved.
func (s *Service) Attach(ctx context.Context, in AttachInput) (Doctor, error) {
	app, err := s.repo.GetApplication(ctx, in.ApplicationID)
	if err != nil {
		return Doctor{}, err
	}
	if app.Status == ApplicationActivated {
		d, err := s.repo.GetByUserID(ctx, in.UserID)
		if err == nil {
			return d, nil
		}
		d, err = s.repo.GetByID(ctx, app.ID)
		if err == nil {
			return d, nil
		}
		return Doctor{}, ErrApplicationNotReady
	}
	if app.Status != ApplicationApproved {
		return Doctor{}, ErrApplicationNotReady
	}
	if _, err := s.repo.GetByUserID(ctx, in.UserID); err == nil {
		return Doctor{}, ErrAlreadyRegistered
	} else if !errors.Is(err, ErrNotFound) {
		return Doctor{}, err
	}

	now := time.Now().UTC()
	email := strings.TrimSpace(in.Email)
	if email == "" {
		email = app.Email
	}
	d := &Doctor{
		ID:                 app.ID,
		UserID:             in.UserID,
		SLMCNumber:         app.SLMCNumber,
		Specialty:          app.Specialty,
		ExperienceYears:    app.ExperienceYears,
		FeeCents:           app.FeeCents,
		Currency:           "LKR",
		DisplayName:        app.DisplayName,
		Languages:          app.Languages,
		Bio:                app.Bio,
		VerificationStatus: StatusApproved,
		VerifiedAt:         &now,
		VerifiedBy:         app.DecidedBy,
		AcceptsNewPatients: true,
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.Create(ctx, tx, d); err != nil {
			return err
		}
		if err := s.repo.MarkApplicationActivated(ctx, tx, app.ID, in.UserID, now); err != nil {
			return err
		}
		payload := events.DoctorApproved{
			DoctorID:   d.ID,
			UserID:     d.UserID,
			DoctorName: d.DisplayName,
			Email:      email,
			Specialty:  d.Specialty,
			FeeCents:   d.FeeCents,
			Currency:   d.Currency,
			Languages:  languagesToStrings(d.Languages),
			ApprovedAt: now,
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorApproved, d.ID.String(), payload)
	})
	if err != nil {
		return Doctor{}, err
	}
	d.CreatedAt = now
	d.UpdatedAt = now
	return *d, nil
}
