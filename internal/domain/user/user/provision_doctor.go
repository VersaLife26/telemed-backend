package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/httpx"
)

// ProvisionDoctorInput is the identity copied from an approved public
// doctor application. PasswordHash is a bcrypt digest produced at apply
// time; empty means an OTP-only account (legacy applications).
type ProvisionDoctorInput struct {
	Email        string
	Phone        string
	Name         string
	PasswordHash string
}

// ProvisionDoctorResult reports what provisioning did, not just who it did it
// to. PasswordApplied is false when the applicant already had an account with
// a password of its own: that password is deliberately left alone (see
// ProvisionDoctor), so the approval email must not tell them to sign in with
// the one they typed on the apply form.
type ProvisionDoctorResult struct {
	User            *User
	PasswordApplied bool
}

// ProvisionDoctor creates or promotes the platform login for an approved
// doctor so they can sign in with email and password (or OTP).
//
// It does not attach the doctors row — doctor-service calls Attach itself
// after this returns, so the two services do not HTTP-recurse.
//
// An existing account keeps its existing password. Overwriting it from an
// application would be an account-takeover path: apply under someone else's
// email, and approval would silently reset their credential. The caller is
// told via PasswordApplied so it can say the right thing instead.
func (s *Service) ProvisionDoctor(ctx context.Context, in ProvisionDoctorInput) (ProvisionDoctorResult, error) {
	email := NormalizeEmail(in.Email)
	if !validEmail(email) {
		return ProvisionDoctorResult{}, ErrInvalidEmail
	}
	phone := httpx.NormalizePhone(in.Phone)
	if phone == "" {
		return ProvisionDoctorResult{}, ErrInvalidPhone
	}
	if in.PasswordHash != "" && !validBcryptHash(in.PasswordHash) {
		return ProvisionDoctorResult{}, ErrInvalidPassword
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = emailLocalPart(email)
	}

	var u *User
	var isNew bool
	var passwordApplied bool
	err := database.InTx(ctx, s.repo.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		byEmail, emailErr := s.repo.FindUserByEmail(ctx, tx, email)
		if emailErr != nil && !errors.Is(emailErr, ErrUserNotFound) {
			return emailErr
		}
		byPhone, phoneErr := s.repo.FindUserByPhone(ctx, tx, phone)
		if phoneErr != nil && !errors.Is(phoneErr, ErrUserNotFound) {
			return phoneErr
		}

		switch {
		case byEmail != nil && byPhone != nil && byEmail.ID != byPhone.ID:
			return fmt.Errorf("%w: email and phone belong to different accounts", ErrEmailTaken)
		case byEmail != nil:
			u = byEmail
		case byPhone != nil:
			u = byPhone
		}

		if u == nil {
			isNew = true
			passwordApplied = in.PasswordHash != ""
			u = &User{
				Phone:        phone,
				Email:        &email,
				Name:         name,
				Language:     LanguageEnglish,
				Role:         RoleDoctor,
				Status:       StatusActive,
				PasswordHash: in.PasswordHash,
			}
			if err := s.repo.CreateUser(ctx, tx, u); err != nil {
				return err
			}
			return s.enqueueRegistered(ctx, tx, u)
		}

		if err := s.refuseIfClosed(u); err != nil {
			return err
		}
		changed := false
		if u.Name == "" && name != "" {
			u.Name = name
			changed = true
		}
		if u.Email == nil || *u.Email == "" {
			u.Email = &email
			changed = true
		}
		if u.Phone == "" {
			u.Phone = phone
			changed = true
		}
		if changed {
			if err := s.repo.UpdateProfile(ctx, tx, u); err != nil {
				return err
			}
		}
		if u.Role != RoleDoctor {
			if err := s.repo.SetRole(ctx, tx, u.ID, RoleDoctor); err != nil {
				return err
			}
			u.Role = RoleDoctor
		}
		if in.PasswordHash != "" {
			hash, err := s.repo.GetPasswordHash(ctx, tx, u.ID)
			if err != nil {
				return err
			}
			if hash == "" {
				if err := s.repo.SetPasswordHash(ctx, tx, u.ID, in.PasswordHash); err != nil {
					return err
				}
				u.PasswordHash = in.PasswordHash
				passwordApplied = true
			}
		}
		return nil
	})
	if err != nil {
		return ProvisionDoctorResult{}, err
	}
	if isNew {
		s.provisionKeycloak(ctx, u)
	}
	return ProvisionDoctorResult{User: u, PasswordApplied: passwordApplied}, nil
}

func validBcryptHash(hash string) bool {
	return strings.HasPrefix(hash, "$2a$") ||
		strings.HasPrefix(hash, "$2b$") ||
		strings.HasPrefix(hash, "$2y$")
}
