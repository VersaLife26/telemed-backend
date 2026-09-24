package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/doctor/analytics"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/storage"
)

// Service holds every business rule for doctor profiles, credentialing,
// availability declarations and reviews. It never sees an *http.Request
// (handler.go's job) and never writes SQL (repository.go's job).
type Service struct {
	repo      *Repository
	pool      database.Pool
	outbox    *events.Outbox
	cache     cache.Cache
	enc       Encryptor
	searchTTL time.Duration
	log       zerolog.Logger

	// holidays forwards a doctor's leave to scheduling-service, which owns the
	// holidays table. Nil means this deployment cannot forward, and a save
	// carrying leave is refused rather than silently accepted -- see
	// holidays.go for why that refusal matters.
	holidays HolidayRegistrar

	// accounts creates the login user when an admin accepts a public
	// application. Nil leaves that to the user-service event consumer.
	accounts AccountActivator

	// store holds signature and seal images in the doctor-credentials bucket.
	store storage.Storage
}

// AccountActivator provisions the doctor login account after credentialing
// accept, so the applicant can sign in without a first OTP and appears in
// admin Users immediately.
type AccountActivator interface {
	ActivateApprovedApplication(ctx context.Context, applicationID uuid.UUID) error
}

// NewService wires the service's dependencies.
func NewService(repo *Repository, pool database.Pool, outbox *events.Outbox, c cache.Cache, enc Encryptor, searchTTL time.Duration, log zerolog.Logger) *Service {
	if searchTTL <= 0 {
		searchTTL = 60 * time.Second
	}
	return &Service{repo: repo, pool: pool, outbox: outbox, cache: c, enc: enc, searchTTL: searchTTL, log: log}
}

// WithHolidayRegistrar attaches the scheduling-service seam and returns the
// service, so boot reads as one expression.
//
// It is a separate builder rather than another NewService parameter because it
// is genuinely optional: a deployment with no SCHEDULING_BASE_URL still serves
// every other endpoint, and the eight positional arguments NewService already
// takes are enough.
func (s *Service) WithHolidayRegistrar(h HolidayRegistrar) *Service {
	s.holidays = h
	return s
}

// SetAccountActivator attaches the user-service seam used on Accept.
func (s *Service) SetAccountActivator(a AccountActivator) {
	s.accounts = a
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// RegisterInput is the validated shape of a doctor registration.
type RegisterInput struct {
	UserID          uuid.UUID
	DisplayName     string
	SLMCNumber      string
	Specialty       string
	SubSpecialties  []string
	ExperienceYears int
	FeeCents        int64
	Languages       []Language
	Bio             string
	PhotoURL        string
	Qualifications  []Qualification
	Bank            *BankDetails
}

// registeredPayload builds the canonical doctor.registered event.
//
// The four credential document keys are carried because admin-service's
// verification queue -- the one consumer that matters here -- cannot review an
// application it cannot open the attachments of. At registration they are
// normally all empty: a doctor registers first and uploads credentials
// afterwards, which is why doctor.documents_updated exists as well. They are
// populated here anyway so that a replay of this event never blanks a
// projection that a later documents event already filled, and so that a future
// registration flow which submits documents up front needs no producer change.
//
// FullName and YearsExperience are on the payload rather than resolved over
// gRPC because they are facts about the APPLICATION, which this service owns.
// Email and phone are not: those are contact details user-service owns and
// admin-service resolves through its directory client (INTEGRATION-FIXES #9).
func registeredPayload(d Doctor, docs events.DoctorCredentialDocuments, at time.Time) events.DoctorRegistered {
	return events.DoctorRegistered{
		DoctorID:                  d.ID,
		UserID:                    d.UserID,
		FullName:                  d.DisplayName,
		SLMCNumber:                d.SLMCNumber,
		Specialty:                 d.Specialty,
		YearsExperience:           d.ExperienceYears,
		DoctorCredentialDocuments: docs,
		CreatedAt:                 at,
	}
}

// documentsUpdatedPayload builds the canonical doctor.documents_updated event.
// It always carries the FULL current key set, never a delta, so a consumer
// that missed a delivery converges on the next one.
func documentsUpdatedPayload(d Doctor, docs events.DoctorCredentialDocuments, at time.Time) events.DoctorDocumentsUpdated {
	return events.DoctorDocumentsUpdated{
		DoctorID:                  d.ID,
		UserID:                    d.UserID,
		DoctorCredentialDocuments: docs,
		UpdatedAt:                 at,
	}
}

// Register creates a new doctor profile in the pending verification state.
// Real SLMC registry verification is a manual admin workflow (see
// docs/DESIGN.md); this call never contacts SLMC and never marks a doctor
// approved.
func (s *Service) Register(ctx context.Context, in RegisterInput) (Doctor, error) {
	if ok, err := s.repo.SpecialtyExists(ctx, in.Specialty); err != nil {
		return Doctor{}, err
	} else if !ok {
		return Doctor{}, fmt.Errorf("%w: unknown specialty %q", ErrInvalidTransition, in.Specialty)
	}
	if _, err := s.repo.GetByUserID(ctx, in.UserID); err == nil {
		return Doctor{}, ErrAlreadyRegistered
	} else if !errors.Is(err, ErrNotFound) {
		return Doctor{}, err
	}

	d := &Doctor{
		ID:                 uuid.New(),
		UserID:             in.UserID,
		SLMCNumber:         in.SLMCNumber,
		Specialty:          in.Specialty,
		SubSpecialties:     in.SubSpecialties,
		ExperienceYears:    in.ExperienceYears,
		FeeCents:           in.FeeCents,
		DisplayName:        in.DisplayName,
		Languages:          in.Languages,
		Bio:                in.Bio,
		PhotoURL:           in.PhotoURL,
		Qualifications:     in.Qualifications,
		VerificationStatus: StatusPending,
		AcceptsNewPatients: true,
	}

	if in.Bank != nil {
		enc, err := s.encryptBank(ctx, in.Bank)
		if err != nil {
			return Doctor{}, err
		}
		d.BankEncrypted = enc
	}

	registeredAt := time.Now().UTC()
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.Create(ctx, tx, d); err != nil {
			return err
		}
		docs, err := s.repo.ListDocumentsTx(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorRegistered, d.ID.String(),
			registeredPayload(*d, CredentialDocumentKeys(docs), registeredAt))
	})
	if err != nil {
		return Doctor{}, err
	}
	d.CreatedAt = registeredAt
	return *d, nil
}

func (s *Service) encryptBank(ctx context.Context, b *BankDetails) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("doctor: marshal bank details: %w", err)
	}
	enc, err := s.enc.Encrypt(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("doctor: encrypt bank details: %w", err)
	}
	return enc, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetPublic returns a doctor profile for the public detail endpoint. A
// non-approved profile is only visible to its own owner: to anyone else it
// 404s exactly like a profile that does not exist, so search cannot be used
// to enumerate doctors still in the verification queue.
func (s *Service) GetPublic(ctx context.Context, id uuid.UUID, callerUserID *uuid.UUID, callerIsAdmin bool) (Doctor, error) {
	d, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Doctor{}, err
	}
	if d.IsApproved() || callerIsAdmin || (callerUserID != nil && *callerUserID == d.UserID) {
		return d, nil
	}
	return Doctor{}, ErrNotFound
}

// GetMine returns the caller's own doctor profile.
func (s *Service) GetMine(ctx context.Context, userID uuid.UUID) (Doctor, error) {
	return s.repo.GetByUserID(ctx, userID)
}

// ResolveDoctorID maps a platform user id onto the doctor id this service's
// tables are keyed on. It satisfies analytics.DoctorResolver.
//
// It goes through the profile rather than reading the token's
// telemed_doctor_id claim, because every other /doctors/me endpoint here does
// the same. An endpoint that trusted a claim the others ignore would answer
// with a different doctor's numbers the first time a token was minted without
// it -- and a doctor shown somebody else's earnings is not a cosmetic bug.
//
// A missing profile is translated into analytics.ErrDoctorProfileNotFound so
// the analytics package can render a 404 without importing this one.
func (s *Service) ResolveDoctorID(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	d, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return uuid.Nil, analytics.ErrDoctorProfileNotFound
		}
		return uuid.Nil, err
	}
	return d.ID, nil
}

// SetProfilePhoto validates and stores the calling doctor's directory portrait.
func (s *Service) SetProfilePhoto(ctx context.Context, userID uuid.UUID, filename string, data []byte) (Doctor, error) {
	if len(data) == 0 {
		return Doctor{}, ErrInvalidProfilePhoto
	}
	if len(data) > MaxProfilePhotoBytes {
		return Doctor{}, ErrProfilePhotoTooLarge
	}
	ext := ProfilePhotoExtension(filename)
	sniffed := SniffProfilePhotoContentType(data)
	if !IsAllowedProfilePhoto(ext, sniffed) {
		return Doctor{}, ErrInvalidProfilePhoto
	}
	owned, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return Doctor{}, err
	}
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		updatedAt, err := s.repo.SetProfilePhoto(ctx, tx, owned.ID, data, sniffed)
		if err != nil {
			return err
		}
		owned.PhotoURL = ProfilePhotoPath(owned.ID, updatedAt)
		owned.Version++
		return nil
	})
	if err != nil {
		return Doctor{}, err
	}
	return s.repo.GetByUserID(ctx, userID)
}

// GetProfilePhoto returns the directory portrait for a doctor id.
func (s *Service) GetProfilePhoto(ctx context.Context, doctorID uuid.UUID) (*ProfilePhoto, error) {
	return s.repo.GetProfilePhoto(ctx, doctorID)
}

// ClearProfilePhoto removes the calling doctor's directory portrait.
func (s *Service) ClearProfilePhoto(ctx context.Context, userID uuid.UUID) (Doctor, error) {
	owned, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return Doctor{}, err
	}
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.repo.ClearProfilePhoto(ctx, tx, owned.ID)
	})
	if err != nil {
		return Doctor{}, err
	}
	return s.repo.GetByUserID(ctx, userID)
}

// ---------------------------------------------------------------------------
// Profile update
// ---------------------------------------------------------------------------

// UpdateInput is the validated shape of a profile self-update.
type UpdateInput struct {
	Specialty          string
	SubSpecialties     []string
	ExperienceYears    int
	FeeCents           int64
	Languages          []Language
	Bio                string
	PhotoURL           string
	Qualifications     []Qualification
	AcceptsNewPatients bool
	Bank               *BankDetails // nil = leave bank details unchanged
}

// UpdateMine applies a profile update for the doctor owned by userID,
// enforcing optimistic concurrency against expectedVersion.
func (s *Service) UpdateMine(ctx context.Context, userID uuid.UUID, expectedVersion int, in UpdateInput) (Doctor, error) {
	if ok, err := s.repo.SpecialtyExists(ctx, in.Specialty); err != nil {
		return Doctor{}, err
	} else if !ok {
		return Doctor{}, fmt.Errorf("%w: unknown specialty %q", ErrInvalidTransition, in.Specialty)
	}

	owned, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return Doctor{}, err
	}

	var encBank *string
	if in.Bank != nil {
		enc, err := s.encryptBank(ctx, in.Bank)
		if err != nil {
			return Doctor{}, err
		}
		encBank = &enc
	}

	var result Doctor
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		current, err := s.repo.GetByIDForUpdate(ctx, tx, owned.ID)
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return ErrVersionConflict
		}
		update := ProfileUpdate{
			Specialty: in.Specialty, SubSpecialties: in.SubSpecialties, ExperienceYears: in.ExperienceYears,
			FeeCents: in.FeeCents, Languages: in.Languages, Bio: in.Bio, PhotoURL: in.PhotoURL,
			Qualifications: in.Qualifications, AcceptsNewPatients: in.AcceptsNewPatients, BankEncrypted: encBank,
		}
		if err := s.repo.Update(ctx, tx, current.ID, expectedVersion, update); err != nil {
			return err
		}
		result = current
		result.Specialty, result.SubSpecialties, result.ExperienceYears = in.Specialty, in.SubSpecialties, in.ExperienceYears
		result.FeeCents, result.Languages, result.Bio, result.PhotoURL = in.FeeCents, in.Languages, in.Bio, in.PhotoURL
		result.Qualifications, result.AcceptsNewPatients = in.Qualifications, in.AcceptsNewPatients
		result.Version++

		// Only publish when something a downstream service prices or filters on
		// actually moved. A bio edit is not a repricing event, and fanning one
		// out to every consumer would make the projection churn for nothing.
		//
		// Enqueued inside the same transaction as the UPDATE (ADR-005): a crash
		// between commit and publish would leave scheduling quoting the old fee
		// forever, with no error anywhere.
		if pricingChanged(current, result) {
			hours, hErr := s.repo.GetWorkingHours(ctx, result.ID)
			if hErr != nil {
				return hErr
			}
			set, sErr := s.repo.GetScheduleSettingsTx(ctx, tx, result.ID)
			if sErr != nil {
				return sErr
			}
			return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorUpdated, result.ID.String(),
				updatedPayload(result, hours, set, time.Now().UTC()))
		}
		return nil
	})
	if err != nil {
		return Doctor{}, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Working hours
// ---------------------------------------------------------------------------

// GetAvailability returns a doctor's declared weekly working hours.
func (s *Service) GetAvailability(ctx context.Context, doctorID uuid.UUID) ([]WorkingHour, error) {
	return s.repo.GetWorkingHours(ctx, doctorID)
}

// GetScheduleSettings returns a doctor's slot-shape declaration, or the
// platform defaults if they have never saved one.
func (s *Service) GetScheduleSettings(ctx context.Context, doctorID uuid.UUID) (ScheduleSettings, error) {
	return s.repo.GetScheduleSettings(ctx, doctorID)
}

// SetAvailability validates and replaces a doctor's weekly schedule, then
// announces it.
//
// The announcement is the point. Replacing the rows here without publishing
// left scheduling-service holding whatever pattern it last heard -- so a doctor
// who added Saturday mornings kept generating no Saturday slots, and one who
// removed Friday kept taking Friday bookings. The write and the event commit
// together (ADR-005), so the two databases cannot disagree.
func (s *Service) SetAvailability(ctx context.Context, doctorID uuid.UUID, hours []WorkingHour, set ScheduleSettings) error {
	return s.SetAvailabilityWithLeave(ctx, AvailabilityInput{
		DoctorID: doctorID, WorkingHours: hours, Settings: set,
	})
}

// AvailabilityInput is one save of the availability editor: the weekly pattern,
// the slot shape, and any leave the doctor added on the same screen.
type AvailabilityInput struct {
	DoctorID     uuid.UUID
	WorkingHours []WorkingHour
	Settings     ScheduleSettings

	// Leave is forwarded to scheduling-service, which owns the holidays table.
	// Empty -- the overwhelmingly common case -- means scheduling-service is
	// not contacted at all.
	Leave []HolidayRequest
	// Bearer is the caller's own access token, forwarded so scheduling-service
	// authorises the doctor itself. It is a credential, not a request: this
	// service holds no machine credential for scheduling and should not.
	Bearer string
}

// SetAvailabilityWithLeave saves the weekly pattern and registers any leave
// that came with it.
//
// ORDER IS THE DESIGN, and it is: validate locally, then forward, then commit.
//
//   - Validating first means a payload with overlapping windows never reaches
//     scheduling-service. Forwarding first would let a request that is about to
//     be rejected for a client-side mistake cancel a patient's consultation on
//     the way.
//   - Forwarding before the local commit means a scheduling-service failure
//     leaves this service's state untouched. The doctor is told the save
//     failed, and it did -- entirely. The opposite order would save the working
//     hours, fail the leave, and report an error against a request that half
//     happened.
//   - The leave upsert is idempotent on (doctor_id, date), so the client
//     retrying the identical body re-applies the days that already landed as
//     no-ops. That is what makes "fail the whole request" a safe policy rather
//     than a trap.
//
// The one window this leaves open is a forward that succeeds and a commit that
// then fails: leave registered against a save the doctor was told failed. It is
// the least bad of the three available orderings -- the leave is a fact the
// doctor explicitly asked for, and a retry converges -- and it is stated here
// rather than discovered.
func (s *Service) SetAvailabilityWithLeave(ctx context.Context, in AvailabilityInput) error {
	doctorID, hours, set := in.DoctorID, in.WorkingHours, in.Settings

	if err := validateNoOverlap(hours); err != nil {
		return err
	}
	set.DoctorID = doctorID
	if set.Timezone == "" {
		set.Timezone = DefaultScheduleTimezone
	}
	if set.SlotDurationMinutes == 0 {
		set.SlotDurationMinutes = DefaultSlotDurationMinutes
	}
	if err := set.Validate(); err != nil {
		return err
	}

	if len(in.Leave) > 0 {
		if s.holidays == nil {
			return ErrHolidayForwardingDisabled
		}
		if err := s.holidays.RegisterHolidays(ctx, in.Bearer, in.Leave); err != nil {
			return err
		}
	}

	return database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.ReplaceWorkingHours(ctx, tx, doctorID, hours); err != nil {
			return err
		}
		// Hours and slot shape are one declaration and commit together. Storing
		// the windows without the shape would leave scheduling slicing the new
		// pattern with the old duration.
		if err := s.repo.UpsertScheduleSettings(ctx, tx, set); err != nil {
			return err
		}

		d, err := s.repo.GetByID(ctx, doctorID)
		if err != nil {
			return err
		}
		// Only an approved doctor is sellable, so only their schedule is worth
		// regenerating slots from.
		if d.VerificationStatus != StatusApproved {
			return nil
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorUpdated, doctorID.String(),
			updatedPayload(d, hours, set, time.Now().UTC()))
	})
}

// validateNoOverlap rejects two windows on the same day that overlap. The
// database UNIQUE(doctor_id, day_of_week, start_time) constraint only
// catches identical start times, not partial overlaps like 09:00-11:00 and
// 10:00-12:00, so this check has to live in the service layer.
func validateNoOverlap(hours []WorkingHour) error {
	byDay := map[int][]WorkingHour{}
	for _, h := range hours {
		byDay[h.DayOfWeek] = append(byDay[h.DayOfWeek], h)
	}
	for _, windows := range byDay {
		sort.Slice(windows, func(i, j int) bool { return windows[i].StartTime < windows[j].StartTime })
		for i := 1; i < len(windows); i++ {
			if windows[i].StartTime < windows[i-1].EndTime {
				return ErrOverlappingHours
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

// UploadDocument records a credential document reference and announces the
// doctor's new document set on doctor.documents_updated.
//
// The storage key is derived from the caller's own doctor id, never taken from
// the request. filename is advisory and contributes at most an allowlisted
// extension. See objectkey.go for why deriving beats validating here.
//
// The event is not optional garnish. Registration and credential upload are
// two separate calls in that order, so doctor.registered is always published
// before any document exists. Without this event the admin verification queue
// -- where a human decides whether a doctor may treat patients -- shows every
// application with an empty document viewer, and credential review is
// impossible through the API.
//
// Insert and enqueue share one transaction (ADR-005), so a document that
// exists here but never reached the reviewer is not a state this code can
// produce.
func (s *Service) UploadDocument(ctx context.Context, doctorID uuid.UUID, docType DocumentType, data []byte) (Document, error) {
	// Signature and seal have their own endpoints, which enforce the formats
	// the prescription PDF can actually embed.
	if docType == DocumentSignature || docType == DocumentSeal {
		return Document{}, ErrInvalidDocumentType
	}
	if s.store == nil {
		return Document{}, ErrCredentialStoreUnavailable
	}
	if len(data) > MaxApplyDocumentBytes {
		return Document{}, ErrDocumentTooLarge
	}
	ext, contentType := credentialDocumentKind(data)
	if ext == "" {
		return Document{}, ErrInvalidCredentialDocument
	}
	d, err := s.repo.GetByID(ctx, doctorID)
	if err != nil {
		return Document{}, err
	}

	now := time.Now().UTC()
	// The key is DERIVED, never accepted. It used to be whatever string the
	// request body contained, so a doctor could submit another doctor's
	// slmc_certificate_key as their own and be approved to practise medicine
	// on it -- see objectkey.go. doctorID here comes from the caller's own
	// profile, looked up from their authenticated principal by the handler.
	doc := &Document{
		ID:           uuid.New(),
		DoctorID:     doctorID,
		DocumentType: docType,
		ObjectKey:    DeriveCredentialKey(doctorID, docType, "upload"+ext),
		UploadedAt:   now,
	}
	// Written before the row: a row whose object is missing is exactly the
	// "reviewer opens nothing" state this upload used to leave behind.
	if err := s.store.Put(ctx, storage.BucketDoctorCredentials, doc.ObjectKey, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
		return Document{}, fmt.Errorf("doctor: store %s document: %w", docType, err)
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.InsertDocument(ctx, tx, doc); err != nil {
			return err
		}
		docs, err := s.repo.ListDocumentsTx(ctx, tx, doctorID)
		if err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorDocumentsUpdated, doctorID.String(),
			documentsUpdatedPayload(d, CredentialDocumentKeys(docs), now))
	})
	if err != nil {
		return Document{}, err
	}
	return *doc, nil
}

// SignatureAndSealKeys resolves a doctor's current signature and seal
// credential object keys, for embedding on an issued prescription PDF.
// Empty strings mean "never uploaded" -- a valid state for the caller to
// act on (e.g. by refusing to issue), not an error condition here.
//
// This is the method the record domain reaches in-process (via
// modular.KeyDoctorCredentialImages) rather than a cross-service database
// read: doctor_documents stays this domain's own table (ADR-004), and only
// the two resolved keys ever cross the boundary.
func (s *Service) SignatureAndSealKeys(ctx context.Context, doctorID uuid.UUID) (signatureKey, sealKey string, err error) {
	docs, err := s.repo.ListDocuments(ctx, doctorID)
	if err != nil {
		return "", "", fmt.Errorf("doctor: list documents: %w", err)
	}
	signatureKey, sealKey = SignatureAndSealKeys(docs)
	return signatureKey, sealKey, nil
}

// ---------------------------------------------------------------------------
// Reviews
// ---------------------------------------------------------------------------

// ListReviews returns a doctor's published reviews, paginated.
func (s *Service) ListReviews(ctx context.Context, doctorID uuid.UUID, page, perPage int) ([]Review, int64, error) {
	return s.repo.ListPublishedReviews(ctx, doctorID, perPage, (page-1)*perPage)
}

// CreateReview records a patient's review for a completed appointment,
// enforced against the review_eligibility projection populated by the
// appointment.completed consumer (see events_consumer.go).
func (s *Service) CreateReview(ctx context.Context, doctorID, patientID, appointmentID uuid.UUID, rating int, comment string) (Review, error) {
	eligible, err := s.repo.IsEligibleForReview(ctx, doctorID, patientID, appointmentID)
	if err != nil {
		return Review{}, err
	}
	if !eligible {
		return Review{}, ErrNotEligibleReview
	}

	rv := &Review{ID: uuid.New(), DoctorID: doctorID, PatientID: patientID, AppointmentID: appointmentID, Rating: rating, Comment: comment, IsPublished: true}
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.repo.InsertReview(ctx, tx, rv); err != nil {
			return err
		}
		return s.repo.RefreshReviewAggregate(ctx, tx, doctorID)
	})
	if err != nil {
		return Review{}, err
	}
	return *rv, nil
}

// ---------------------------------------------------------------------------
// Search (with Redis caching)
// ---------------------------------------------------------------------------

type searchCachePayload struct {
	Results []SearchResult `json:"results"`
	Total   int64          `json:"total"`
}

// Search resolves a doctor search, caching whole result pages in Redis for
// searchTTL keyed by a hash of the normalised query -- see docs/DESIGN.md.
func (s *Service) Search(ctx context.Context, f SearchFilters) ([]SearchResult, int64, error) {
	key := searchCacheKey(f)

	if raw, err := s.cache.Get(ctx, key); err == nil {
		var payload searchCachePayload
		if jerr := json.Unmarshal(raw, &payload); jerr == nil {
			return payload.Results, payload.Total, nil
		}
		// A corrupt cache entry must not fail the request; fall through to
		// the database and let the next Set overwrite it.
	} else if !errors.Is(err, cache.ErrNotFound) {
		s.log.Warn().Err(err).Msg("doctor search: cache get failed, querying database")
	}

	results, total, err := s.repo.Search(ctx, f)
	if err != nil {
		return nil, 0, err
	}

	if raw, jerr := json.Marshal(searchCachePayload{Results: results, Total: total}); jerr == nil {
		if err := s.cache.Set(ctx, key, raw, s.searchTTL); err != nil {
			s.log.Warn().Err(err).Msg("doctor search: cache set failed")
		}
	}
	return results, total, nil
}

// searchCacheKey hashes the normalised filter set. Two requests that mean
// the same search (e.g. page=1 vs an omitted page defaulting to 1, already
// normalised by the handler before this point) must hash identically, or
// the cache buys nothing.
func searchCacheKey(f SearchFilters) string {
	h := sha256.New()
	// hash.Hash's Write never returns an error, which is why the result is
	// discarded explicitly rather than ignored implicitly.
	_, _ = fmt.Fprintf(h, "specialty=%s|language=%s|min_fee=%v|max_fee=%v|min_rating=%v|q=%s|available_after=%s|require_available=%t|sort=%s|page=%d|per_page=%d",
		f.Specialty, f.Language, ptrInt64(f.MinFeeCents), ptrInt64(f.MaxFeeCents), ptrFloat64(f.MinRating), f.Query,
		f.AvailableAfter.UTC().Format(time.RFC3339), f.RequireAvailable, f.SortBy, f.Page, f.PerPage)
	return "doctors:search:" + hex.EncodeToString(h.Sum(nil))
}

func ptrInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *p)
}

func ptrFloat64(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *p)
}

// ---------------------------------------------------------------------------
// Verification (internal, admin-service facing)
// ---------------------------------------------------------------------------

// ListPending returns the credentialing queue: doctors in pending or
// under_review, with their uploaded documents attached.
func (s *Service) ListPending(ctx context.Context, page, perPage int) ([]PendingDoctor, int64, error) {
	docs, total, err := s.repo.ListPending(ctx, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	out := make([]PendingDoctor, len(docs))
	for i := range docs {
		documents, err := s.repo.ListDocuments(ctx, docs[i].ID)
		if err != nil {
			return nil, 0, err
		}
		out[i] = PendingDoctor{Doctor: docs[i], Documents: documents}
	}
	return out, total, nil
}

// The doctor.approved / doctor.rejected / doctor.updated payloads used to be
// declared here, privately. They are now events.DoctorApproved,
// events.DoctorRejected and events.DoctorUpdated -- the same structs every
// consumer imports, so a field this service forgets to send is a compile
// error rather than a zero value nobody notices.
//
// This is not a hypothetical improvement. The private doctorApprovedPayload
// carried {doctor_id, user_id} and nothing else, while scheduling-service
// needed the fee to quote a price and notification-service needed the name and
// email to address the approval. Both got empty values, silently, and no
// patient could pay.

// approvedPayload builds the canonical doctor.approved event from a doctor row.
//
// Email is deliberately EMPTY. doctor-service has no email column and no
// user-service client: the address lives in telemed_user and this service has
// never held it. Fabricating a lookup that does not exist would be worse than
// an absent field, so the field is omitempty and notification-service resolves
// the address itself. Recorded in the report and in docs/DESIGN.md.
func approvedPayload(d Doctor, hours []WorkingHour, set ScheduleSettings, approvedAt time.Time) events.DoctorApproved {
	p := events.DoctorApproved{
		DoctorID:   d.ID,
		UserID:     d.UserID,
		DoctorName: d.DisplayName,
		Specialty:  d.Specialty,
		FeeCents:   d.FeeCents,
		Currency:   defaultCurrency(d.Currency),
		Languages:  languagesToStrings(d.Languages),
		// Without these, scheduling-service creates default schedule settings,
		// generates zero slots, and the doctor is approved but permanently
		// unbookable -- silently.
		WorkingHours: toEventWorkingHours(hours),
		ApprovedAt:   approvedAt,
	}
	applyScheduleSettings(set,
		&p.Timezone, &p.SlotDurationMinutes, &p.BufferMinutes, &p.MaxPerDay)
	return p
}

// applyScheduleSettings copies a doctor's slot-shape declaration onto an event.
//
// The pointers are the event's own fields; writing through them keeps the two
// payload builders from drifting apart, which is how doctor.approved ended up
// carrying working hours while doctor.updated did not.
//
// buffer is passed through as a POINTER, nil included. Every other field is
// omitempty-with-a-zero-meaning-unset, which is fine because a zero-minute
// consultation is not a thing anyone means. A zero-minute BUFFER is: it means
// back-to-back. So nil has to survive all the way onto the wire, where the
// omitempty tag turns it into an absent field and scheduling-service's
// consumer -- which reads it as a *int for exactly this reason -- keeps its own
// default instead of being told "zero".
func applyScheduleSettings(set ScheduleSettings, tz *string, slot *int, buffer **int, maxPerDay *int) {
	*tz = set.Timezone
	if *tz == "" {
		*tz = scheduleTimezone
	}
	*slot = set.SlotDurationMinutes
	*buffer = set.BufferMinutes
	*maxPerDay = set.MaxPerDay
}

// scheduleTimezone is the wall-clock zone the working hours are expressed in.
// Slot generation resolves it through tzdata, never as a fixed offset.
const scheduleTimezone = DefaultScheduleTimezone

// toEventWorkingHours converts the stored rows into the canonical wire shape.
// Postgres TIME renders as "HH:MM:SS"; the event carries "HH:MM" because the
// seconds are always zero and the consumer parses wall-clock, not duration.
func toEventWorkingHours(hours []WorkingHour) []events.WorkingHour {
	if len(hours) == 0 {
		return nil
	}
	out := make([]events.WorkingHour, 0, len(hours))
	for _, h := range hours {
		avail := h.IsAvailable
		out = append(out, events.WorkingHour{
			DayOfWeek:   h.DayOfWeek,
			StartTime:   trimSeconds(h.StartTime),
			EndTime:     trimSeconds(h.EndTime),
			IsAvailable: &avail,
		})
	}
	return out
}

func trimSeconds(t string) string {
	if len(t) >= 5 {
		return t[:5]
	}
	return t
}

// updatedPayload builds the canonical doctor.updated event. This is what keeps
// scheduling-service's pricing projection current: a doctor who raises their
// fee changes the list price for FUTURE bookings only, because scheduling has
// already stamped the quote onto every appointment booked at the old price.
func updatedPayload(d Doctor, hours []WorkingHour, set ScheduleSettings, updatedAt time.Time) events.DoctorUpdated {
	p := events.DoctorUpdated{
		DoctorID:     d.ID,
		Specialty:    d.Specialty,
		FeeCents:     d.FeeCents,
		Currency:     defaultCurrency(d.Currency),
		Languages:    languagesToStrings(d.Languages),
		Status:       string(d.VerificationStatus),
		UpdatedAt:    updatedAt,
		WorkingHours: toEventWorkingHours(hours),
	}
	applyScheduleSettings(set,
		&p.Timezone, &p.SlotDurationMinutes, &p.BufferMinutes, &p.MaxPerDay)
	return p
}

// pricingChanged reports whether an update touched anything scheduling-service
// prices or filters on. A doctor rewriting their bio must not cost the platform
// an event fan-out; a doctor changing their fee must.
func pricingChanged(before, after Doctor) bool {
	if before.FeeCents != after.FeeCents ||
		before.Specialty != after.Specialty ||
		before.Currency != after.Currency ||
		before.VerificationStatus != after.VerificationStatus {
		return true
	}
	return !sameLanguages(before.Languages, after.Languages)
}

// sameLanguages compares two language lists order-insensitively: reordering
// ["en","si"] to ["si","en"] is not a change worth an event.
func sameLanguages(a, b []Language) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[Language]int, len(a))
	for _, l := range a {
		seen[l]++
	}
	for _, l := range b {
		seen[l]--
		if seen[l] < 0 {
			return false
		}
	}
	return true
}

// Verify performs one admin-initiated verification workflow transition. The
// legality of the transition is decided entirely by nextVerificationStatus
// (verification.go) -- this method's job is validating the reason
// requirement, applying the write, and publishing the domain event for the
// two transitions the platform declares subjects for (approved, rejected).
func (s *Service) Verify(ctx context.Context, doctorID uuid.UUID, action VerificationAction, reason string, actorID *uuid.UUID) (Doctor, error) {
	if reasonRequiredActions[action] && reason == "" {
		return Doctor{}, ErrReasonRequired
	}

	var result Doctor
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		current, err := s.repo.GetByIDForUpdate(ctx, tx, doctorID)
		if err != nil {
			return err
		}
		next, err := nextVerificationStatus(current.VerificationStatus, action)
		if err != nil {
			return err
		}
		if err := s.repo.UpdateVerificationStatus(ctx, tx, doctorID, current.Version, next, reason, actorID); err != nil {
			return err
		}

		result = current
		result.VerificationStatus = next
		result.Version++
		if next == StatusRejected {
			result.RejectionReason = reason
		}
		if next == StatusApproved {
			approvedAt := time.Now().UTC()
			result.VerifiedAt = &approvedAt
			result.VerifiedBy = actorID
		}

		// Loaded once, inside the same transaction, so the event carries the
		// availability that was true at the moment of the decision.
		hours, hErr := s.repo.GetWorkingHours(ctx, doctorID)
		if hErr != nil {
			return hErr
		}
		set, sErr := s.repo.GetScheduleSettingsTx(ctx, tx, doctorID)
		if sErr != nil {
			return sErr
		}

		now := time.Now().UTC()
		switch next {
		case StatusApproved:
			// Carries the fee and specialty. scheduling-service cannot quote a
			// booking without them and must not call back here on the booking
			// hot path.
			return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorApproved, doctorID.String(),
				approvedPayload(result, hours, set, now))
		case StatusRejected:
			return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorRejected, doctorID.String(),
				events.DoctorRejected{
					DoctorID:   doctorID,
					UserID:     current.UserID,
					DoctorName: current.DisplayName,
					Reason:     reason,
					RejectedAt: now,
				})
		default:
			// start_review / reopen / suspend / reinstate. These used to be
			// unpublished, because no subject existed for them. doctor.updated
			// now does: a suspended doctor whose pricing row still says
			// "approved" in scheduling is a doctor the platform keeps selling.
			return s.outbox.Enqueue(ctx, tx, events.SubjectDoctorUpdated, doctorID.String(),
				updatedPayload(result, hours, set, now))
		}
	})
	if err != nil {
		return Doctor{}, err
	}
	return result, nil
}
