package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
)

func applicationReadyToActivate(app DoctorApplication) bool {
	return app.Status == "approved" || app.Status == "activated"
}

// ErrDoctorApplicationNotReady is returned when activation is asked for an
// application that is still pending or otherwise not eligible.
var ErrDoctorApplicationNotReady = errors.New("user: doctor application is not ready")

func loginDoctorAccountError(err error) error {
	if errors.Is(err, ErrPhoneTaken) || errors.Is(err, ErrEmailTaken) {
		return ErrInvalidCredentials
	}
	return err
}

func (s *Service) lookupDoctorApplicationByEmail(ctx context.Context, email string) (DoctorApplication, bool) {
	if s.doctors == nil {
		return DoctorApplication{}, false
	}
	app, err := s.doctors.ApplicationByEmail(ctx, email)
	if err == nil {
		return app, true
	}
	if !errors.Is(err, ErrDoctorApplicationNotFound) {
		s.log.Warn().Err(err).Msg("doctor application by email lookup failed; continuing with account login")
	}
	return DoctorApplication{}, false
}

// promoteUserFromApplication copies identity from an approved application onto
// an existing account: display name, missing email/phone, password hash, doctor role.
func (s *Service) promoteUserFromApplication(ctx context.Context, tx pgx.Tx, u *User, app DoctorApplication) error {
	changed := false
	if u.Name == "" && app.DisplayName != "" {
		u.Name = app.DisplayName
		changed = true
	}
	if (u.Email == nil || *u.Email == "") && app.Email != "" {
		email := NormalizeEmail(app.Email)
		u.Email = &email
		changed = true
	}
	if u.Phone == "" && app.Phone != "" {
		u.Phone = app.Phone
		changed = true
	}
	if changed {
		if err := s.repo.UpdateProfile(ctx, tx, u); err != nil {
			return err
		}
	}
	if app.PasswordHash != "" {
		existing, err := s.repo.GetPasswordHash(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		if existing == "" {
			if err := s.repo.SetPasswordHash(ctx, tx, u.ID, app.PasswordHash); err != nil {
				return err
			}
			u.PasswordHash = app.PasswordHash
		}
	}
	if u.Role != RoleDoctor {
		if err := s.repo.SetRole(ctx, tx, u.ID, RoleDoctor); err != nil {
			return err
		}
		u.Role = RoleDoctor
	}
	return nil
}

func (s *Service) createUserFromApplication(ctx context.Context, tx pgx.Tx, app DoctorApplication) (*User, error) {
	var emailPtr *string
	if email := NormalizeEmail(app.Email); email != "" {
		emailPtr = &email
	}
	u := &User{
		Phone:        app.Phone,
		Name:         app.DisplayName,
		Email:        emailPtr,
		PasswordHash: app.PasswordHash,
		Language:     LanguageEnglish,
		Role:         RoleDoctor,
		Status:       StatusActive,
	}
	if err := s.repo.CreateUser(ctx, tx, u); err != nil {
		return nil, err
	}
	if err := s.enqueueRegistered(ctx, tx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Service) attachApprovedDoctor(ctx context.Context, app DoctorApplication, u *User) error {
	if s.doctors == nil || app.Status != "approved" {
		return nil
	}
	email := app.Email
	if u.Email != nil && *u.Email != "" {
		email = *u.Email
	}
	if err := s.doctors.Attach(ctx, app.ID, u.ID, email); err != nil {
		s.log.Error().Err(err).
			Str("user_id", logger.MaskID(u.ID.String())).
			Msg("failed to attach approved doctor application")
		return fmt.Errorf("user: activate doctor profile: %w", err)
	}
	return nil
}

func emailsCompatible(u *User, app DoctorApplication) bool {
	if u.Email == nil || *u.Email == "" {
		return true
	}
	return NormalizeEmail(*u.Email) == NormalizeEmail(app.Email)
}

// ActivateApprovedApplication creates the doctor login from an approved
// public application. Called synchronously from doctor-service on admin Accept
// and from the application_approved consumer.
func (s *Service) ActivateApprovedApplication(ctx context.Context, applicationID uuid.UUID) error {
	if s.doctors == nil {
		return fmt.Errorf("user: doctor applications not configured")
	}
	if applicationID == uuid.Nil {
		return fmt.Errorf("user: application id is required")
	}
	app, err := s.doctors.ApplicationByID(ctx, applicationID)
	if err != nil {
		return err
	}
	if !applicationReadyToActivate(app) {
		return fmt.Errorf("%w: %s", ErrDoctorApplicationNotReady, app.Status)
	}
	_, err = s.ensureDoctorAccount(ctx, app)
	return err
}

// ensureDoctorAccount creates or promotes the user for an approved application
// and attaches the doctor profile. Idempotent once the application is activated.
func (s *Service) ensureDoctorAccount(ctx context.Context, app DoctorApplication) (*User, error) {
	if s.doctors == nil {
		return nil, fmt.Errorf("user: doctor applications not configured")
	}
	if !applicationReadyToActivate(app) {
		return nil, fmt.Errorf("user: doctor application is %s", app.Status)
	}

	email := NormalizeEmail(app.Email)
	var u *User
	var isNew bool
	err := database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ferr := s.repo.FindUserByEmail(ctx, tx, email)
		if errors.Is(ferr, ErrUserNotFound) && app.Phone != "" {
			byPhone, perr := s.repo.FindUserByPhone(ctx, tx, app.Phone)
			switch {
			case perr == nil && emailsCompatible(byPhone, app):
				existing, ferr = byPhone, nil
			case perr == nil:
				return ErrPhoneTaken
			case !errors.Is(perr, ErrUserNotFound):
				return perr
			}
		}
		switch {
		case ferr == nil:
			u = existing
			return s.promoteUserFromApplication(ctx, tx, u, app)
		case errors.Is(ferr, ErrUserNotFound):
			created, cerr := s.createUserFromApplication(ctx, tx, app)
			if cerr != nil {
				return cerr
			}
			isNew = true
			u = created
			return nil
		default:
			return ferr
		}
	})
	if err != nil {
		return nil, err
	}
	if err := s.refuseIfClosed(u); err != nil {
		return nil, err
	}
	if err := s.attachApprovedDoctor(ctx, app, u); err != nil {
		return nil, err
	}
	if isNew {
		s.provisionKeycloak(ctx, u)
	}
	return u, nil
}

// ApplicationApprovedConsumer provisions a doctor user when credentialing
// approves a public application, so the account appears in admin Users and
// email/password login works without a prior OTP.
type ApplicationApprovedConsumer struct {
	svc *Service
	log zerolog.Logger
}

// NewApplicationApprovedConsumer builds the consumer.
func NewApplicationApprovedConsumer(svc *Service, log zerolog.Logger) *ApplicationApprovedConsumer {
	return &ApplicationApprovedConsumer{svc: svc, log: log.With().Str("component", "doctor_application_approved_consumer").Logger()}
}

// Subscribe registers the durable consumer. Call once at boot; it blocks until
// ctx is cancelled, so run it in its own goroutine like the outbox relay.
func (c *ApplicationApprovedConsumer) Subscribe(ctx context.Context, sub events.Subscriber) error {
	return sub.Subscribe(ctx, "user-doctor-application-approved",
		[]events.Subject{events.SubjectDoctorApplicationApproved},
		c.Handle)
}

// Handle creates the doctor user for an approved application. Redelivery after
// attach is a no-op: the application is activated and the user already exists.
func (c *ApplicationApprovedConsumer) Handle(ctx context.Context, env events.Envelope) error {
	if c.svc == nil || c.svc.doctors == nil {
		return nil
	}
	var payload events.DoctorApplicationApproved
	if err := env.Decode(&payload); err != nil {
		return fmt.Errorf("user: decode %s: %w", env.Subject, err)
	}
	if payload.ApplicationID == uuid.Nil {
		c.log.Warn().Str("subject", string(env.Subject)).Msg("doctor.application_approved missing application_id; acknowledging")
		return nil
	}
	err := c.svc.ActivateApprovedApplication(ctx, payload.ApplicationID)
	if errors.Is(err, ErrDoctorApplicationNotReady) {
		return nil
	}
	if errors.Is(err, ErrUserSuspended) || errors.Is(err, ErrUserDeleted) || errors.Is(err, ErrPhoneTaken) {
		c.log.Warn().Err(err).
			Str("application_id", payload.ApplicationID.String()).
			Msg("doctor application approved but account could not be provisioned; acknowledging")
		return nil
	}
	return err
}
