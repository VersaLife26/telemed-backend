package clinicalnotes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Service implements the clinical-note rules: autosave, finalisation,
// amendment, authorised reads, and ICD-10 search.
type Service struct {
	repo    *Repository
	pool    database.Pool
	fhirCli fhir.Client
	outbox  *events.Outbox
	access  *access.Service
	log     zerolog.Logger
}

// NewService wires the clinical notes service.
func NewService(repo *Repository, pool database.Pool, fhirCli fhir.Client,
	outbox *events.Outbox, accessSvc *access.Service, log zerolog.Logger,
) *Service {
	return &Service{
		repo: repo, pool: pool, fhirCli: fhirCli, outbox: outbox, access: accessSvc,
		log: log.With().Str("component", "clinical_notes").Logger(),
	}
}

// Sentinel errors for state the caller must distinguish. They are translated
// into the HTTP taxonomy at the edge of this file, never returned raw.
var (
	// errFinalised is a draft save against a note that has since been
	// signed. It maps to NOTE_FINALISED (409), NOT to CONFLICT, because a
	// client that retries a CONFLICT would retry this one forever.
	errFinalised = errors.New("clinicalnotes: note is finalised")
	// errNotFinalised is an amendment of something that was never signed.
	errNotFinalised = errors.New("clinicalnotes: note is not finalised")
	// errNoChange is an amendment that changes nothing. A revision recording
	// no change is noise in a legal trail.
	errNoChange = errors.New("clinicalnotes: amendment does not change anything")
	// errEmpty is finalising a note with no content in any section.
	errEmpty = errors.New("clinicalnotes: note has no content")
	// errNotFound is used inside transactions where httpx types do not belong.
	errNotFound = errors.New("clinicalnotes: note not found")
)

// DiagnosisInput is one ICD-10 code as the client sends it. Only the code is
// trusted: the display term is always re-resolved from icd10_codes, so a
// client cannot store "E11.9 — patient is malingering" as a diagnosis
// rubric.
type DiagnosisInput struct {
	Code      string
	IsPrimary bool
}

// SaveInput is one autosave.
type SaveInput struct {
	Principal     middleware.Principal
	AppointmentID uuid.UUID
	Subjective    string
	Objective     string
	Assessment    string
	Plan          string
	Diagnoses     []DiagnosisInput
	// Version is the optimistic-lock token the client last received. Zero
	// means "I believe no note exists yet"; if one does, that is a lost
	// update waiting to happen and the save is refused with a 409.
	Version int
}

// Save is the autosave upsert behind PUT /clinical-notes/{appointment_id}.
//
// Three properties it has to have, because the doctor app calls it on a
// debounce after every keystroke, from a device that may be one of two the
// same doctor has open:
//
//  1. Cheap. One authorisation read, one note read, one diagnosis read, and
//     a single UPDATE. The diagnosis table is only rewritten when the
//     diagnosis set actually changed, which on a typing burst is never.
//  2. Idempotent. Sending the same content twice is not an edit: the second
//     call short-circuits before the UPDATE and returns the same version.
//     Without that, a retried request would advance the version and turn the
//     other device's in-flight save into a spurious conflict.
//  3. Safe under concurrency. Every write is guarded by (id, version) AND by
//     status = 'draft', so a save racing a finalisation on another device
//     cannot overwrite a signed record; it loses cleanly with a 409.
func (s *Service) Save(ctx context.Context, in SaveInput) (Note, error) {
	rel, err := s.authoriseWrite(ctx, in.Principal, in.AppointmentID)
	if err != nil {
		return Note{}, err
	}
	if err := validateSections(in.Subjective, in.Objective, in.Assessment, in.Plan); err != nil {
		return Note{}, err
	}
	incoming, err := normaliseDiagnoses(in.Diagnoses)
	if err != nil {
		return Note{}, err
	}

	var out Note
	var conflictVersion int
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ok, err := s.repo.GetByAppointment(ctx, tx, in.AppointmentID)
		if err != nil {
			return err
		}

		if !ok {
			// First save for this appointment. A non-zero version means the
			// client thinks it is updating a note that is not there -- most
			// likely it is talking to a database that was restored, and
			// silently creating a fresh note would hide that.
			if in.Version != 0 {
				return database.ErrOptimisticLock
			}
			resolved, err := s.resolveDiagnoses(ctx, tx, incoming)
			if err != nil {
				return err
			}
			created, err := s.repo.Create(ctx, tx, Note{
				ID: uuid.New(), AppointmentID: in.AppointmentID,
				DoctorID: rel.DoctorID, PatientID: rel.PatientID,
				Subjective: in.Subjective, Objective: in.Objective,
				Assessment: in.Assessment, Plan: in.Plan,
			})
			if err != nil {
				return err
			}
			if len(resolved) > 0 {
				if err := s.repo.ReplaceDiagnoses(ctx, tx, created.ID, resolved); err != nil {
					return err
				}
			}
			created.Diagnoses = resolved
			out = created
			return nil
		}

		conflictVersion = existing.Version
		if existing.DoctorID != in.Principal.DoctorID {
			return httpx.ErrForbidden.WithCause(fmt.Errorf("clinicalnotes: note %s belongs to another doctor", existing.ID))
		}
		if existing.IsFinalised() {
			return errFinalised
		}
		if in.Version != existing.Version {
			return database.ErrOptimisticLock
		}

		current, err := s.repo.ListDiagnoses(ctx, tx, existing.ID)
		if err != nil {
			return err
		}
		existing.Diagnoses = current

		// Resolving display terms costs a query, so it is skipped entirely
		// when the code set is unchanged -- which it is for every keystroke
		// in a text field, i.e. almost every call this endpoint ever gets.
		resolved := current
		if !sameCodes(current, incoming) {
			resolved, err = s.resolveDiagnoses(ctx, tx, incoming)
			if err != nil {
				return err
			}
		}

		candidate := existing
		candidate.Subjective, candidate.Objective = in.Subjective, in.Objective
		candidate.Assessment, candidate.Plan = in.Assessment, in.Plan
		candidate.Diagnoses = resolved

		if existing.SameContentAs(candidate) {
			out = existing
			return nil
		}

		updated, err := s.repo.UpdateDraft(ctx, tx, existing.ID, existing.Version, candidate)
		if err != nil {
			return err
		}
		if !sameDiagnoses(current, resolved) {
			if err := s.repo.ReplaceDiagnoses(ctx, tx, updated.ID, resolved); err != nil {
				return err
			}
		}
		updated.Diagnoses = resolved
		out = updated
		return nil
	})
	if err != nil {
		return Note{}, s.translate(err, in.AppointmentID, conflictVersion)
	}
	return out, nil
}

// FinaliseInput signs a draft off.
type FinaliseInput struct {
	Principal     middleware.Principal
	AppointmentID uuid.UUID
	Version       int
	IPAddress     string
	UserAgent     string
}

// Finalise turns a draft into a legal medical record.
//
// Everything that must be true at once happens in one transaction: the
// status flip, revision 1 capturing the text exactly as signed, and the
// outbox row announcing it. If any of the three fails, none of them
// happened -- there is no state where a note is finalised with no revision
// recording what it said, and none where the event escaped without the note
// being signed.
func (s *Service) Finalise(ctx context.Context, in FinaliseInput) (Note, error) {
	if _, err := s.authoriseWrite(ctx, in.Principal, in.AppointmentID); err != nil {
		return Note{}, err
	}

	var out Note
	var revisionNo int
	var conflictVersion int
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ok, err := s.repo.GetByAppointment(ctx, tx, in.AppointmentID)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound
		}
		conflictVersion = existing.Version
		if existing.DoctorID != in.Principal.DoctorID {
			return httpx.ErrForbidden.WithCause(fmt.Errorf("clinicalnotes: note %s belongs to another doctor", existing.ID))
		}
		if existing.IsFinalised() {
			return errFinalised
		}
		if in.Version != existing.Version {
			return database.ErrOptimisticLock
		}
		if !existing.HasContent() {
			return errEmpty
		}

		diagnoses, err := s.repo.ListDiagnoses(ctx, tx, existing.ID)
		if err != nil {
			return err
		}

		// Truncated to microseconds because Postgres TIMESTAMPTZ stores
		// microseconds: a nanosecond-precision value here and the value
		// Postgres hands back on the next read would differ, and this
		// timestamp is the moment a doctor signed a legal record.
		at := time.Now().UTC().Truncate(time.Microsecond)
		finalised, err := s.repo.Finalise(ctx, tx, existing.ID, existing.Version, at)
		if err != nil {
			return err
		}
		finalised.Diagnoses = diagnoses

		rev, err := s.repo.InsertRevision(ctx, tx, Revision{
			NoteID:     finalised.ID,
			Subjective: finalised.Subjective, Objective: finalised.Objective,
			Assessment: finalised.Assessment, Plan: finalised.Plan,
			Diagnoses: diagnoses, ChangeType: ChangeFinalise,
			ChangedBy: in.Principal.UserID, ChangedByRole: primaryRole(in.Principal),
		})
		if err != nil {
			return err
		}
		revisionNo = rev.Revision
		out = finalised

		// Identifiers and a count. No SOAP text, and no ICD-10 codes: a code
		// IS a diagnosis, and this event fans out to every consumer on the
		// bus. Anything that needs the content fetches it over the API and
		// takes the access-log entry that comes with it.
		return s.outbox.Enqueue(ctx, tx, events.SubjectClinicalNoteFinalised, finalised.ID.String(),
			events.ClinicalNoteFinalised{
				NoteID: finalised.ID, AppointmentID: finalised.AppointmentID,
				PatientID: finalised.PatientID, DoctorID: finalised.DoctorID,
				DiagnosisCount: len(diagnoses), FinalisedAt: at,
			})
	})
	if err != nil {
		return Note{}, s.translate(err, in.AppointmentID, conflictVersion)
	}

	s.logAccess(ctx, in.Principal, out, access.ActionFinalise, in.IPAddress, in.UserAgent)
	s.syncFHIR(ctx, out, revisionNo)
	return out, nil
}

// AmendInput changes a note that has already been signed. Every section is a
// pointer: an amendment names only what it changes, and nil means "leave it
// exactly as the doctor signed it" rather than "blank it".
type AmendInput struct {
	Principal     middleware.Principal
	AppointmentID uuid.UUID
	Subjective    *string
	Objective     *string
	Assessment    *string
	Plan          *string
	Diagnoses     *[]DiagnosisInput
	Reason        string
	Version       int
	IPAddress     string
	UserAgent     string
}

// Amend changes a finalised note without destroying what it said before.
//
// The live row is updated and a new revision is appended carrying the note's
// content as it now stands. The previous revision -- and, for the first
// amendment, revision 1, the text as originally signed -- is untouched and
// unreachable by any UPDATE or DELETE the application can issue: the
// database refuses both via trigger. That is the whole point of the table,
// and it is why "amend" is the only way a finalised note can change.
func (s *Service) Amend(ctx context.Context, in AmendInput) (Note, error) {
	if _, err := s.authoriseWrite(ctx, in.Principal, in.AppointmentID); err != nil {
		return Note{}, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return Note{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"amendment_reason is required: an unexplained change to a signed medical record is not auditable")
	}
	if len([]rune(reason)) > MaxAmendmentReasonRunes {
		return Note{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			fmt.Sprintf("amendment_reason must be at most %d characters", MaxAmendmentReasonRunes))
	}

	var out Note
	var revisionNo int
	var conflictVersion int
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, ok, err := s.repo.GetByAppointment(ctx, tx, in.AppointmentID)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound
		}
		conflictVersion = existing.Version
		if existing.DoctorID != in.Principal.DoctorID {
			return httpx.ErrForbidden.WithCause(fmt.Errorf("clinicalnotes: note %s belongs to another doctor", existing.ID))
		}
		if !existing.IsFinalised() {
			return errNotFinalised
		}
		if in.Version != existing.Version {
			return database.ErrOptimisticLock
		}

		current, err := s.repo.ListDiagnoses(ctx, tx, existing.ID)
		if err != nil {
			return err
		}
		existing.Diagnoses = current

		candidate := existing
		candidate.Subjective = valueOr(in.Subjective, existing.Subjective)
		candidate.Objective = valueOr(in.Objective, existing.Objective)
		candidate.Assessment = valueOr(in.Assessment, existing.Assessment)
		candidate.Plan = valueOr(in.Plan, existing.Plan)
		if err := validateSections(candidate.Subjective, candidate.Objective, candidate.Assessment, candidate.Plan); err != nil {
			return err
		}

		resolved := current
		if in.Diagnoses != nil {
			incoming, err := normaliseDiagnoses(*in.Diagnoses)
			if err != nil {
				return err
			}
			if !sameCodes(current, incoming) {
				resolved, err = s.resolveDiagnoses(ctx, tx, incoming)
				if err != nil {
					return err
				}
			}
		}
		candidate.Diagnoses = resolved

		if existing.SameContentAs(candidate) {
			return errNoChange
		}
		if !candidate.HasContent() {
			return errEmpty
		}

		updated, err := s.repo.AmendFinalised(ctx, tx, existing.ID, existing.Version, candidate)
		if err != nil {
			return err
		}
		if !sameDiagnoses(current, resolved) {
			if err := s.repo.ReplaceDiagnoses(ctx, tx, updated.ID, resolved); err != nil {
				return err
			}
		}
		updated.Diagnoses = resolved

		rev, err := s.repo.InsertRevision(ctx, tx, Revision{
			NoteID:     updated.ID,
			Subjective: updated.Subjective, Objective: updated.Objective,
			Assessment: updated.Assessment, Plan: updated.Plan,
			Diagnoses: resolved, ChangeType: ChangeAmend, AmendmentReason: reason,
			ChangedBy: in.Principal.UserID, ChangedByRole: primaryRole(in.Principal),
		})
		if err != nil {
			return err
		}
		revisionNo = rev.Revision
		out = updated
		return nil
	})
	if err != nil {
		return Note{}, s.translate(err, in.AppointmentID, conflictVersion)
	}

	s.logAccess(ctx, in.Principal, out, access.ActionAmend, in.IPAddress, in.UserAgent)
	s.syncFHIR(ctx, out, revisionNo)
	return out, nil
}

// Get returns one note, enforcing the read matrix and writing the audit
// trail entry.
//
// Two rules, in this order:
//
//  1. A DRAFT is visible only to its author. A patient reading a
//     half-finished differential -- "? lymphoma, ? TB" -- before the doctor
//     has finished thinking is a real harm, and the note is not yet a record
//     of anything. Non-authors get 404, not 403: from the patient's point of
//     view there genuinely is no note for this consultation yet, and 403
//     would tell them one exists.
//  2. Everything else goes through access.Service.Check, the same matrix
//     records and prescriptions use -- with the one difference that
//     administrators are refused clinical notes outright (see
//     access.decideAccess).
//
// Both outcomes are written to document_access_log; there is no path out of
// this function that reads a note without leaving a trace.
func (s *Service) Get(ctx context.Context, caller middleware.Principal, appointmentID uuid.UUID, ip, ua string) (Note, error) {
	note, ok, err := s.repo.GetByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return Note{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Note{}, httpx.ErrNotFound
	}

	if !note.IsFinalised() && !isAuthor(caller, note) {
		s.logDenied(ctx, caller, note, access.ActionView, access.ReasonDeniedDraft, ip, ua)
		return Note{}, httpx.ErrNotFound
	}

	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: note.PatientID, Resource: access.ResourceClinicalNote,
		ResourceID: note.ID, Action: access.ActionView, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return Note{}, err
	}

	diagnoses, err := s.repo.ListDiagnoses(ctx, s.pool, note.ID)
	if err != nil {
		return Note{}, httpx.ErrInternal.WithCause(err)
	}
	note.Diagnoses = diagnoses
	return note, nil
}

// ListForDoctor returns the calling doctor's own notes, paginated.
//
// Deliberately carries no clinical content -- no SOAP text, no diagnosis
// codes, just which consultations have a note and whether it is signed. That
// is what makes it safe to serve without a per-note access-log entry: there
// is nothing here to leak. Opening any one of them goes through Get and is
// logged.
func (s *Service) ListForDoctor(ctx context.Context, caller middleware.Principal, status Status, page, perPage int) ([]Note, int64, error) {
	if !caller.HasRole(middleware.RoleDoctor) || caller.DoctorID == uuid.Nil {
		return nil, 0, httpx.ErrForbidden.WithCause(errors.New("clinicalnotes: only a doctor may list their own notes"))
	}
	if status != "" && status != StatusDraft && status != StatusFinalised {
		return nil, 0, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "status must be draft or finalised")
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	notes, total, err := s.repo.ListByDoctor(ctx, s.pool, ListFilter{
		DoctorID: caller.DoctorID, Status: status, Page: page, PerPage: perPage,
	})
	if err != nil {
		return nil, 0, httpx.ErrInternal.WithCause(err)
	}
	return notes, total, nil
}

// ListRevisions returns the amendment trail for a note. Same authorisation
// as Get: a patient may see how their own record changed, the treating
// doctor may see what they signed, an administrator may see neither.
func (s *Service) ListRevisions(ctx context.Context, caller middleware.Principal, appointmentID uuid.UUID, ip, ua string) ([]Revision, error) {
	note, ok, err := s.repo.GetByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return nil, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return nil, httpx.ErrNotFound
	}
	if !note.IsFinalised() && !isAuthor(caller, note) {
		s.logDenied(ctx, caller, note, access.ActionList, access.ReasonDeniedDraft, ip, ua)
		return nil, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: note.PatientID, Resource: access.ResourceClinicalNote,
		ResourceID: note.ID, Action: access.ActionList, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return nil, err
	}
	revisions, err := s.repo.ListRevisions(ctx, s.pool, note.ID)
	if err != nil {
		return nil, httpx.ErrInternal.WithCause(err)
	}
	return revisions, nil
}

// icd10CodePrefix recognises input that is the beginning of an ICD-10 code
// rather than the beginning of a word: a letter, then one or two digits,
// then optionally a dot and up to two more. "E11" and "E11.9" match; "E"
// does not (one letter would drag in a whole chapter), and neither does
// "eye".
var icd10CodePrefix = regexp.MustCompile(`^[A-Za-z]\d{1,2}\.?\d{0,2}$`)

// tsQueryToken keeps only what a Postgres text-search lexeme may contain, so
// the query string handed to to_tsquery cannot carry an operator.
var tsQueryToken = regexp.MustCompile(`[^a-z0-9]+`)

// SearchICD10 backs the diagnosis picker.
//
// A one-character query returns an empty list rather than an error: the
// client debounces but still fires while a doctor is typing, and a search
// box that flashes a 400 at you mid-word is worse than one that shows
// nothing for a moment. An entirely empty q is a different thing -- a
// programming error in the caller -- and is a 400, matching /drugs.
func (s *Service) SearchICD10(ctx context.Context, query string, limit int) ([]ICD10Code, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "q is required")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if len([]rune(query)) < 2 {
		return []ICD10Code{}, nil
	}

	var codePrefix string
	if icd10CodePrefix.MatchString(query) {
		codePrefix = strings.ToUpper(query)
	}
	tsQuery := buildTSQuery(query)
	if codePrefix == "" && tsQuery == "" {
		return []ICD10Code{}, nil
	}

	codes, err := s.repo.SearchICD10(ctx, s.pool, codePrefix, tsQuery, limit)
	if err != nil {
		return nil, httpx.ErrInternal.WithCause(err)
	}
	return codes, nil
}

// buildTSQuery turns free text into a prefix tsquery: "dengue haem" becomes
// "dengue:* & haem:*", which is what makes the picker respond usefully while
// the doctor is still typing the word.
//
// Every token is stripped to [a-z0-9] first. That is not decoration: it is
// what guarantees the resulting string cannot contain a tsquery operator,
// and it is why passing this to to_tsquery as a bound parameter is safe
// rather than merely conventional.
func buildTSQuery(query string) string {
	fields := tsQueryToken.Split(strings.ToLower(query), -1)
	tokens := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		tokens = append(tokens, f+":*")
	}
	return strings.Join(tokens, " & ")
}

// --- authorisation and audit helpers ------------------------------------

// authoriseWrite establishes that the caller is the doctor who is treating
// (or has treated) the patient in this appointment.
//
// Writes take this path rather than access.Service.Check on purpose. Check
// answers "may this principal READ this patient's record", and its answer
// includes doctors holding a patient-granted share -- correct for reading a
// record, wrong for writing one. Only the treating doctor authors the note
// for their own consultation, so the write rule is the narrower one, taken
// from the same treating_relationships read-model Check uses and the same
// one prescriptions.Issue uses to verify appointment ownership.
//
// Unlike prescriptions, this does NOT require the consultation to have
// ended. A doctor types the note while the call is running; requiring
// consultation.ended would make the feature unusable exactly when it is
// used.
func (s *Service) authoriseWrite(ctx context.Context, p middleware.Principal, appointmentID uuid.UUID) (access.TreatingRelationship, error) {
	if !p.HasRole(middleware.RoleDoctor) || p.DoctorID == uuid.Nil {
		return access.TreatingRelationship{}, httpx.ErrForbidden.WithCause(
			errors.New("clinicalnotes: only the treating doctor may write a clinical note"))
	}
	rel, ok, err := s.access.Repository().TreatingRelationshipByAppointment(ctx, s.access.Pool(), appointmentID)
	if err != nil {
		return access.TreatingRelationship{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok || rel.DoctorID != p.DoctorID {
		return access.TreatingRelationship{}, httpx.ErrForbidden.WithCause(
			fmt.Errorf("clinicalnotes: caller does not treat appointment %s", appointmentID))
	}
	return rel, nil
}

// isAuthor reports whether the caller is the doctor whose note this is.
func isAuthor(p middleware.Principal, n Note) bool {
	return p.HasRole(middleware.RoleDoctor) && p.DoctorID != uuid.Nil && p.DoctorID == n.DoctorID
}

// primaryRole is the role string recorded on a revision and on an access-log
// entry. It matches what access.Service.Check records, so the two trails can
// be read together.
func primaryRole(p middleware.Principal) string {
	if len(p.Roles) == 0 {
		return "anonymous"
	}
	return string(p.Roles[0])
}

// logAccess records a granted, legally significant act against the note. A
// failure to write it is logged loudly but does not undo an act that has
// already committed -- the revision row is the durable record either way.
func (s *Service) logAccess(ctx context.Context, p middleware.Principal, n Note, action access.Action, ip, ua string) {
	userID := p.UserID
	entry := access.LogEntry{
		ResourceType: access.ResourceClinicalNote, ResourceID: n.ID, OwnerUserID: n.PatientID,
		AccessedBy: &userID, AccessedByRole: primaryRole(p), Action: action,
		Granted: true, Reason: access.ReasonTreatingDoctor, IPAddress: ip, UserAgent: ua,
	}
	if err := s.access.Repository().InsertAccessLog(ctx, s.access.Pool(), entry); err != nil {
		s.log.Error().Err(err).Str("note_id", n.ID.String()).Str("action", string(action)).
			Msg("failed to write clinical note access log entry")
	}
}

// logDenied records a refusal that happened before access.Check could run --
// the draft-visibility rule. Without this, the one denial the matrix does
// not itself produce would be the only unlogged outcome in the service.
func (s *Service) logDenied(ctx context.Context, p middleware.Principal, n Note, action access.Action, reason access.Reason, ip, ua string) {
	entry := access.LogEntry{
		ResourceType: access.ResourceClinicalNote, ResourceID: n.ID, OwnerUserID: n.PatientID,
		AccessedByRole: primaryRole(p), Action: action, Granted: false, Reason: reason,
		IPAddress: ip, UserAgent: ua,
	}
	if p.UserID != uuid.Nil {
		id := p.UserID
		entry.AccessedBy = &id
	}
	if err := s.access.Repository().InsertAccessLog(ctx, s.access.Pool(), entry); err != nil {
		s.log.Error().Err(err).Str("note_id", n.ID.String()).Msg("failed to write clinical note denial log entry")
	}
}

// --- validation ---------------------------------------------------------

// validateSections bounds each SOAP section. The limit is also a CHECK
// constraint in migration 000004; enforcing it here too is what turns "one
// of your sections is too long" into a message naming the section.
func validateSections(subjective, objective, assessment, plan string) error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"subjective", subjective}, {"objective", objective},
		{"assessment", assessment}, {"plan", plan},
	} {
		if len([]rune(f.value)) > MaxSectionRunes {
			// The message names the field and its limit and quotes none of
			// its content -- an error string is a log line waiting to happen.
			return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
				fmt.Sprintf("%s must be at most %d characters", f.name, MaxSectionRunes))
		}
	}
	return nil
}

// normaliseDiagnoses uppercases and trims codes, rejects duplicates and
// multiple primaries, and caps the list. It performs no I/O so the whole
// rule is unit-testable.
func normaliseDiagnoses(in []DiagnosisInput) ([]DiagnosisInput, error) {
	if len(in) > MaxDiagnoses {
		return nil, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			fmt.Sprintf("at most %d diagnoses may be attached to one note", MaxDiagnoses))
	}
	out := make([]DiagnosisInput, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	primaries := 0
	for _, d := range in {
		code := strings.ToUpper(strings.TrimSpace(d.Code))
		if code == "" {
			return nil, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "a diagnosis code cannot be empty")
		}
		if _, dup := seen[code]; dup {
			return nil, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
				fmt.Sprintf("diagnosis code %s appears more than once", code))
		}
		seen[code] = struct{}{}
		if d.IsPrimary {
			primaries++
		}
		out = append(out, DiagnosisInput{Code: code, IsPrimary: d.IsPrimary})
	}
	if primaries > 1 {
		return nil, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"only one diagnosis may be marked primary")
	}
	return out, nil
}

// resolveDiagnoses turns client-supplied codes into stored diagnoses,
// resolving each display term from icd10_codes rather than trusting the
// client's.
//
// An unknown code is rejected, not stored. A free-text "diagnosis" smuggled
// in through the code field would be PHI in a column the whole platform
// treats as a controlled vocabulary, and it would silently break every
// report that groups by code.
func (s *Service) resolveDiagnoses(ctx context.Context, db dbtx, in []DiagnosisInput) ([]Diagnosis, error) {
	if len(in) == 0 {
		return nil, nil
	}
	codes := make([]string, len(in))
	for i, d := range in {
		codes[i] = d.Code
	}
	known, err := s.repo.LookupICD10(ctx, db, codes)
	if err != nil {
		return nil, err
	}
	out := make([]Diagnosis, 0, len(in))
	for i, d := range in {
		ref, ok := known[d.Code]
		if !ok {
			return nil, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
				fmt.Sprintf("%s is not a known ICD-10 code", d.Code))
		}
		out = append(out, Diagnosis{Code: ref.Code, Display: ref.Display, IsPrimary: d.IsPrimary, SortOrder: i})
	}
	return out, nil
}

// sameCodes reports whether a stored diagnosis list and an incoming one
// carry the same codes, in the same order, with the same primary flags. It
// is what lets Save skip the reference-table lookup on a keystroke.
func sameCodes(stored []Diagnosis, incoming []DiagnosisInput) bool {
	if len(stored) != len(incoming) {
		return false
	}
	for i := range stored {
		if stored[i].Code != incoming[i].Code || stored[i].IsPrimary != incoming[i].IsPrimary {
			return false
		}
	}
	return true
}

// sameDiagnoses reports whether two stored diagnosis lists are identical.
func sameDiagnoses(a, b []Diagnosis) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code || a[i].Display != b[i].Display || a[i].IsPrimary != b[i].IsPrimary {
			return false
		}
	}
	return true
}

func valueOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// translate converts repository and sentinel errors into the shared HTTP
// taxonomy. It is the only place in the package that decides a status code,
// and it never puts note content into a message.
func (s *Service) translate(err error, appointmentID uuid.UUID, currentVersion int) error {
	apiErr := &httpx.APIError{}
	if errors.As(err, &apiErr) {
		return err
	}
	switch {
	case errors.Is(err, database.ErrOptimisticLock):
		conflict := httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"this note was changed on another device; reload it and apply your change again")
		if currentVersion > 0 {
			// Handing back the version the server actually holds saves the
			// client a round trip in the case this error exists for: two
			// devices, one doctor, both mid-sentence.
			conflict.Fields = map[string]string{"version": strconv.Itoa(currentVersion)}
		}
		return conflict.WithCause(err)
	case errors.Is(err, errFinalised):
		return httpx.NewError(http.StatusConflict, httpx.CodeNoteFinalised,
			"this note has been finalised; changes must be made as an amendment").WithCause(err)
	case errors.Is(err, errNotFinalised):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"only a finalised note can be amended; this one is still a draft").WithCause(err)
	case errors.Is(err, errNoChange):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"the amendment does not change anything").WithCause(err)
	case errors.Is(err, errEmpty):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"a clinical note must have content in at least one section before it can be finalised").WithCause(err)
	case errors.Is(err, errNotFound):
		return httpx.ErrNotFound
	default:
		s.log.Error().Err(err).Str("appointment_id", appointmentID.String()).Msg("clinical note write failed")
		return httpx.ErrInternal.WithCause(err)
	}
}
