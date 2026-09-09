package access

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Service is the authorization crux described in AGENT-BRIEF: "a doctor may
// read a patient's record only during or after an appointment with them, or
// via an explicit unexpired share. Enforce that centrally." Every domain
// package (records, prescriptions) that guards a patient's data calls
// Service.Check instead of writing its own ownership condition, and every
// call -- granted or denied -- is written to the append-only access log by
// the same code path, so it is not possible to add a new read path that
// forgets to audit itself.
type Service struct {
	repo *Repository
	pool database.Pool
	log  zerolog.Logger
}

// NewService constructs the authorization service.
func NewService(repo *Repository, pool database.Pool, log zerolog.Logger) *Service {
	return &Service{repo: repo, pool: pool, log: log.With().Str("component", "access").Logger()}
}

// CheckOptions carries the request-scoped facts the decision and the audit
// log both need.
type CheckOptions struct {
	Principal   middleware.Principal
	OwnerUserID uuid.UUID // the patient whose vault is being accessed
	Resource    ResourceType
	ResourceID  uuid.UUID
	Action      Action
	IPAddress   string
	UserAgent   string
}

// Check decides whether Principal may perform Action on the given resource
// and unconditionally writes the decision to document_access_log before
// returning. Callers must treat a returned *httpx.APIError as final --
// there is no path that grants access without also being logged, and no
// path that logs "granted" without actually being allowed to proceed.
func (s *Service) Check(ctx context.Context, o CheckOptions) error {
	decision := s.decide(ctx, o.Principal, o.OwnerUserID, o.Resource, o.Action)

	entry := LogEntry{
		ResourceType: o.Resource,
		ResourceID:   o.ResourceID,
		OwnerUserID:  o.OwnerUserID,
		Action:       o.Action,
		Granted:      decision.Granted,
		Reason:       decision.Reason,
		IPAddress:    o.IPAddress,
		UserAgent:    o.UserAgent,
	}
	if o.Principal.UserID != uuid.Nil {
		id := o.Principal.UserID
		entry.AccessedBy = &id
	}
	if len(o.Principal.Roles) > 0 {
		entry.AccessedByRole = string(o.Principal.Roles[0])
	} else {
		entry.AccessedByRole = "anonymous"
	}

	if err := s.repo.InsertAccessLog(ctx, s.pool, entry); err != nil {
		// The audit trail does not fail open.
		//
		// This used to log the failure and serve the document anyway, on the
		// reasoning that a logging outage should not become the difference
		// between "allowed" and "denied" for a legitimate caller. That is a
		// defensible trade for a request log. It is not a defensible trade
		// for this log: HIPAA 164.312(b) requires a record of every access
		// to electronic PHI, and an access log that silently misses entries
		// is not an access log -- it is a log that tells an incident
		// responder "nobody read that record" when somebody did. A breach
		// investigation that cannot enumerate who saw a patient's file is
		// the failure mode this table exists to prevent, and it is worse
		// than a 503.
		//
		// The availability cost is small and mostly imaginary. Every caller
		// of Check has already read the document row out of the same pool,
		// so a database outage has failed the request before it reaches
		// here. What actually lands in this branch is a constraint
		// violation or a malformed column value -- precisely the class of
		// bug that must be loud rather than absorbed. (The one shipped
		// example: trimIP returned "[::1]" with brackets, the inet cast
		// rejected it, and on a dual-stack deployment EVERY PHI access went
		// unlogged while every request succeeded. Nobody noticed, because
		// nothing failed.)
		//
		// Denied decisions fail the same way rather than falling through to
		// 403, so the invariant is one sentence with no exceptions: no
		// authorisation decision on PHI is served unless it was durably
		// recorded first.
		s.log.Error().Err(err).
			Str("audit", "phi_access_log_write_failed").
			Str("resource_type", string(o.Resource)).
			Str("resource_id", o.ResourceID.String()).
			Str("action", string(o.Action)).
			Bool("decision_granted", decision.Granted).
			Msg("PHI access denied: the append-only access log could not be written")
		return httpx.ErrUnavailable.WithCause(
			fmt.Errorf("access: refusing to serve %s %s without an audit record: %w", o.Action, o.Resource, err))
	}

	if !decision.Granted {
		return httpx.ErrForbidden.WithCause(fmt.Errorf("access: denied (%s)", decision.Reason))
	}
	return nil
}

// decide performs the I/O (treating-relationship and share lookups) needed
// to reach a decision and delegates the actual rule to decideAccess, which
// has no dependencies and is what access_test.go exercises exhaustively.
func (s *Service) decide(ctx context.Context, p middleware.Principal, ownerUserID uuid.UUID, resource ResourceType, action Action) Decision {
	if p.HasRole(middleware.RoleDoctor) && p.DoctorID != uuid.Nil && p.UserID != ownerUserID && !p.IsAdmin() {
		treated, err := s.repo.HasActiveTreatingRelationship(ctx, s.pool, p.DoctorID, ownerUserID, time.Now().UTC())
		if err != nil {
			s.log.Error().Err(err).Msg("treating relationship lookup failed, denying by default")
			return Decision{Granted: false, Reason: ReasonDeniedNoLink}
		}
		var share *RecordShare
		if !treated {
			if active, ok, err := s.repo.ActiveShare(ctx, s.pool, p.DoctorID, ownerUserID, time.Now().UTC()); err != nil {
				s.log.Error().Err(err).Msg("share lookup failed, denying by default")
				return Decision{Granted: false, Reason: ReasonDeniedNoLink}
			} else if ok {
				share = &active
			}
		}
		return decideAccess(p, ownerUserID, resource, action, treated, share)
	}
	return decideAccess(p, ownerUserID, resource, action, false, nil)
}

// decideAccess is the pure authorization rule described in AGENT-BRIEF: "a
// doctor may read a patient's record only during or after an appointment
// with them, or via an explicit unexpired share." It takes every fact it
// needs as a parameter and performs no I/O, which is what makes it possible
// to exercise the full owner / treating-doctor / untreating-doctor / admin /
// anonymous matrix in a unit test with no database.
//
// treated and activeShare are precomputed by the caller (decide, backed by
// the database) rather than looked up here on demand, so this function's
// only job is the decision, not the fetching.
func decideAccess(p middleware.Principal, ownerUserID uuid.UUID, resource ResourceType, action Action, treated bool, activeShare *RecordShare) Decision {
	if p.UserID == uuid.Nil && p.DoctorID == uuid.Nil {
		return Decision{Granted: false, Reason: ReasonDeniedAnon}
	}

	// The patient always sees their own vault.
	if p.UserID == ownerUserID {
		return Decision{Granted: true, Reason: ReasonOwner}
	}

	// No administrator role reads a patient's clinical record. Not
	// documents, not prescriptions, not clinical notes; not admin, not
	// super_admin, not ops, finance or support. There is deliberately no
	// override flag, because an override flag is a thing that gets set
	// during an incident and never unset.
	//
	// This used to grant every one of middleware.AdminRoles a blanket yes on
	// everything except clinical notes, justified in a comment as reading
	// "the INDEX" so ops could answer "did this patient's report actually
	// upload". The code did not do that. records.Download passes
	// ResourceDocument, so the same grant minted a presigned MinIO URL to
	// the actual lab report, scan or discharge summary -- and
	// prescriptions.Get returned full drug lines, where the drug names the
	// condition. The route carries auth: "authenticated", not auth:
	// "admin", so the gateway's IP allowlist and admin-origin check never
	// applied to it: any of the five roles, from any address on the
	// internet, could read any patient's file. That directly contradicted
	// the platform's own §15 compliance claim that an administrator never
	// sees clinical data (SECURITY-REVIEW F4).
	//
	// The one thing an administrator does legitimately read here is a
	// DOCTOR's credential paperwork -- the credentialing queue cannot
	// verify an SLMC registration without opening the certificate. That is
	// a different bucket, a different owner and not patient data, so it is
	// a different ResourceType rather than an exception carved into this
	// one. If a support workflow genuinely needs a patient's index, it
	// belongs on an admin-service route behind the IP allowlist and the
	// hash-chained audit log, not on the patient API surface.
	if p.IsAdmin() {
		// The credential grant is a READ grant, and only a read grant. It
		// exists so the credentialing queue can open the SLMC certificate a
		// doctor uploaded to prove their registration. Reusing it for a write
		// let any of the five admin roles POST /records/upload with
		// owner_user_id=<any victim> and document_type=credential and plant a
		// file in that person's record index -- or a forged certificate under
		// a real doctor's identity.
		if resource == ResourceCredentialDocument && !action.IsWrite() {
			return Decision{Granted: true, Reason: ReasonAdmin}
		}
		if action.IsWrite() {
			return Decision{Granted: false, Reason: ReasonDeniedWrite}
		}
		return Decision{Granted: false, Reason: ReasonDeniedAdminClinical}
	}

	if p.HasRole(middleware.RoleDoctor) && p.DoctorID != uuid.Nil {
		if treated {
			// A treating doctor legitimately files a lab result or a scan
			// into their patient's vault mid-consultation. That is the ONLY
			// write the relationship carries, and it is stated as an
			// allowlist rather than as exclusions so a write action added
			// later arrives denied rather than granted.
			//
			// Specifically not included: uploading a CREDENTIAL (a doctor's
			// own registration paperwork, a different bucket, and a patient
			// has none), and DELETING anything. Removing a document from a
			// medical record is the patient's decision, and a treating
			// relationship survives the consultation by design -- a doctor
			// who could delete would be able to do so for 30 days afterwards.
			// The branch ignored the resource and the action entirely, so
			// both were free.
			if action.IsWrite() && (action != ActionUpload || resource != ResourceDocument) {
				return Decision{Granted: false, Reason: ReasonDeniedWrite}
			}
			return Decision{Granted: true, Reason: ReasonTreatingDoctor}
		}
		if activeShare != nil {
			// A share is a patient handing a doctor a key to READ. It is
			// created by the patient, for a doctor who is not treating them,
			// and nothing in that gesture says "and you may add documents to
			// my chart".
			if action.IsWrite() {
				return Decision{Granted: false, Reason: ReasonDeniedWrite}
			}
			return Decision{Granted: true, Reason: ReasonShare, ShareID: activeShare.ID}
		}
		return Decision{Granted: false, Reason: ReasonDeniedNoLink}
	}

	return Decision{Granted: false, Reason: ReasonDeniedRole}
}

// CreateShare lets a patient grant a doctor time-boxed access to their
// vault. Only the patient themself (or an admin acting on their behalf) may
// create a share for that patient.
func (s *Service) CreateShare(ctx context.Context, caller middleware.Principal, patientID, doctorID uuid.UUID, ttl time.Duration) (RecordShare, error) {
	if caller.UserID != patientID && !caller.IsAdmin() {
		return RecordShare{}, httpx.ErrForbidden.WithCause(fmt.Errorf("access: only the patient may share their own vault"))
	}
	if ttl <= 0 {
		return RecordShare{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "expires_at must be in the future")
	}
	if ttl > MaxShareTTL {
		return RecordShare{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, fmt.Sprintf("a share cannot exceed %s", MaxShareTTL))
	}
	if patientID == doctorID {
		return RecordShare{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "patient and doctor cannot be the same identity")
	}

	share := RecordShare{
		ID:        uuid.New(),
		PatientID: patientID,
		DoctorID:  doctorID,
		GrantedBy: caller.UserID,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	out, err := s.repo.CreateShare(ctx, s.pool, share)
	if err != nil {
		return RecordShare{}, httpx.ErrInternal.WithCause(err)
	}
	return out, nil
}

// RevokeShare lets the granting patient (or an admin) end a share early.
func (s *Service) RevokeShare(ctx context.Context, caller middleware.Principal, shareID uuid.UUID) error {
	share, ok, err := s.repo.GetShare(ctx, s.pool, shareID)
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return httpx.ErrNotFound
	}
	if caller.UserID != share.PatientID && !caller.IsAdmin() {
		return httpx.ErrForbidden
	}
	if share.RevokedAt != nil {
		return nil // idempotent: already revoked
	}

	revoked, err := s.repo.RevokeShare(ctx, s.pool, shareID, share.Version)
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}
	if !revoked {
		return httpx.ErrConflict.WithCause(fmt.Errorf("access: share %s changed concurrently", shareID))
	}
	return nil
}

// ListShares returns every share a patient has granted, for their own
// review screen.
func (s *Service) ListShares(ctx context.Context, caller middleware.Principal, patientID uuid.UUID) ([]RecordShare, error) {
	if caller.UserID != patientID && !caller.IsAdmin() {
		return nil, httpx.ErrForbidden
	}
	shares, err := s.repo.ListSharesByPatient(ctx, s.pool, patientID)
	if err != nil {
		return nil, httpx.ErrInternal.WithCause(err)
	}
	return shares, nil
}

// Repository exposes the underlying repository for other domain services
// that need direct treating-relationship lookups outside of an access
// decision, e.g. prescriptions.Service verifying appointment ownership at
// issuance time.
func (s *Service) Repository() *Repository { return s.repo }

// Pool exposes the pool for the same reason.
func (s *Service) Pool() database.Pool { return s.pool }
