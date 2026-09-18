package doctor

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"telemed/internal/domain/admin/credentialing"
	"telemed/internal/domain/doctor/doctor"
	"telemed/internal/domain/user/user"
)

// InProcessDoctorApplications bridges the doctor domain to the user domain in-process.
type InProcessDoctorApplications struct {
	svc *doctor.Service
}

// NewInProcessDoctorApplications constructs the in-process adapter.
func NewInProcessDoctorApplications(svc *doctor.Service) *InProcessDoctorApplications {
	return &InProcessDoctorApplications{svc: svc}
}

func (a *InProcessDoctorApplications) ApplicationByPhone(ctx context.Context, phone string) (user.DoctorApplication, error) {
	app, err := a.svc.GetApplicationByPhone(ctx, phone)
	if errors.Is(err, doctor.ErrApplicationNotFound) {
		return user.DoctorApplication{}, user.ErrDoctorApplicationNotFound
	}
	if err != nil {
		return user.DoctorApplication{}, err
	}
	return toUserDoctorApplication(app), nil
}

func (a *InProcessDoctorApplications) ApplicationByEmail(ctx context.Context, email string) (user.DoctorApplication, error) {
	app, err := a.svc.GetApplicationByEmail(ctx, email)
	if errors.Is(err, doctor.ErrApplicationNotFound) {
		return user.DoctorApplication{}, user.ErrDoctorApplicationNotFound
	}
	if err != nil {
		return user.DoctorApplication{}, err
	}
	return toUserDoctorApplication(app), nil
}

func (a *InProcessDoctorApplications) ApplicationByID(ctx context.Context, id uuid.UUID) (user.DoctorApplication, error) {
	app, err := a.svc.GetApplication(ctx, id)
	if errors.Is(err, doctor.ErrApplicationNotFound) {
		return user.DoctorApplication{}, user.ErrDoctorApplicationNotFound
	}
	if err != nil {
		return user.DoctorApplication{}, err
	}
	return toUserDoctorApplication(app), nil
}

func (a *InProcessDoctorApplications) Attach(ctx context.Context, applicationID, userID uuid.UUID, email string) error {
	_, err := a.svc.Attach(ctx, doctor.AttachInput{
		ApplicationID: applicationID,
		UserID:        userID,
		Email:         email,
	})
	return err
}

func (a *InProcessDoctorApplications) DoctorIDByUserID(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	return a.svc.ResolveDoctorID(ctx, userID)
}

func toUserDoctorApplication(app doctor.Application) user.DoctorApplication {
	return user.DoctorApplication{
		ID:           app.ID,
		Status:       string(app.Status),
		DisplayName:  app.DisplayName,
		Email:        app.Email,
		Phone:        app.Phone,
		PasswordHash: app.PasswordHash,
	}
}

// InProcessApplicationVerifier bridges the doctor domain to the admin credentialing domain in-process.
type InProcessApplicationVerifier struct {
	svc *doctor.Service
}

// NewInProcessApplicationVerifier constructs the in-process verifier adapter.
func NewInProcessApplicationVerifier(svc *doctor.Service) *InProcessApplicationVerifier {
	return &InProcessApplicationVerifier{svc: svc}
}

func (v *InProcessApplicationVerifier) VerifyApplication(ctx context.Context, applicationID uuid.UUID, approve bool, reason string) error {
	_, err := v.svc.VerifyApplication(ctx, applicationID, approve, reason, nil)
	if errors.Is(err, doctor.ErrApplicationNotFound) {
		return credentialing.ErrApplicationNotFound
	}
	return err
}

func (v *InProcessApplicationVerifier) ListPendingApplications(ctx context.Context) ([]credentialing.PendingApplication, error) {
	apps, _, err := v.svc.ListPendingApplications(ctx, 1, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]credentialing.PendingApplication, len(apps))
	for i := range apps {
		a := &apps[i]
		out[i] = credentialing.PendingApplication{
			ID:              a.ID,
			FullName:        a.DisplayName,
			Email:           a.Email,
			Phone:           a.Phone,
			SLMCNumber:      a.SLMCNumber,
			Specialty:       a.Specialty,
			YearsExperience: a.ExperienceYears,
			FeeCents:        a.FeeCents,
			CreatedAt:       a.CreatedAt,
		}
	}
	return out, nil
}
