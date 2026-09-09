package records

import (
	"mime/multipart"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/record/access"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Handler wires HTTP to Service. It decodes and validates requests, calls
// the service, and translates results back to the shared JSON envelope --
// no SQL and no business rule lives here (AGENT-BRIEF layering rule).
type Handler struct {
	svc    *Service
	access *access.Service
}

// NewHandler builds the records HTTP handler.
func NewHandler(svc *Service, accessSvc *access.Service) *Handler {
	return &Handler{svc: svc, access: accessSvc}
}

// Routes mounts the /records/* tree. Callers mount this under /api/v1/records
// behind RequireAuth.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/upload", h.upload)
	r.Get("/", h.list)
	r.Get("/shares", h.listShares)
	r.Get("/{id}", h.get)
	r.Get("/{id}/download", h.download)
	r.Delete("/{id}", h.delete)
	r.Post("/{id}/share", h.share)
	return r
}

// ShareRoutes mounts the top-level /shares/{id} resource (revoke), which is
// not nested under /records because a share is not owned by any one
// document.
func (h *Handler) ShareRoutes() chi.Router {
	r := chi.NewRouter()
	r.Delete("/{id}", h.revokeShare)
	return r
}

const maxUploadRequestBytes = MaxUploadBytes + (1 << 20) // file + multipart overhead

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	// The body is already bounded by MaxBytesReader immediately above, so
	// ParseMultipartForm cannot read past maxUploadRequestBytes regardless
	// of what value is passed as its own in-memory-part threshold.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
	if err := r.ParseMultipartForm(maxUploadRequestBytes); err != nil { //nolint:gosec // bounded by MaxBytesReader above
		httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "upload exceeds the request size limit").WithCause(err))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	docType := DocumentType(r.FormValue("document_type"))
	var ownerID uuid.UUID
	if raw := r.FormValue("owner_user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "owner_user_id must be a valid UUID"))
			return
		}
		ownerID = id
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "multipart field \"file\" is required").WithCause(err))
		return
	}
	defer closeMultipart(file)

	doc, err := h.svc.Upload(r.Context(), UploadInput{
		Principal: p, OwnerUserID: ownerID, DocumentType: docType,
		Filename: header.Filename, Data: file,
		IPAddress: middleware.ClientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toDocumentResponse(doc))
}

func closeMultipart(f multipart.File) { _ = f.Close() }

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	ownerID := p.UserID
	if raw, ok, err := httpx.QueryUUID(r, "owner_user_id"); err != nil {
		httpx.Error(w, r, err)
		return
	} else if ok {
		ownerID = raw
	}
	docType := DocumentType(r.URL.Query().Get("document_type"))
	if docType != "" && !ValidDocumentTypes[docType] {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "invalid document_type filter"))
		return
	}
	page, perPage, _ := httpx.Pagination(r)

	docs, total, err := h.svc.List(r.Context(), p, ownerID, docType, page, perPage, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]documentResponse, len(docs))
	for i := range docs {
		out[i] = toDocumentResponse(docs[i])
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doc, err := h.svc.Get(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDocumentResponse(doc))
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	url, doc, err := h.svc.Download(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{
		"download_url":       url,
		"expires_in_seconds": int(recordPresignTTLSeconds),
		"filename":           doc.Filename,
		"content_type":       doc.ContentType,
	})
}

const recordPresignTTLSeconds = 15 * 60

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// shareRequest grants the doctor identified by the {id} path parameter
// time-boxed access to the caller's vault. There is no per-document sharing
// in this schema (record_shares is vault-scoped, matching AGENT-BRIEF's "a
// patient grants a doctor time-boxed access to their vault"), so {id} here
// is the target doctor's user id rather than a document id.
type shareRequest struct {
	TTLHours int `json:"ttl_hours" validate:"required,min=1,max=2160"` // 2160h = 90 days = access.MaxShareTTL
}

func (h *Handler) share(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	doctorID, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req shareRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	share, err := h.access.CreateShare(r.Context(), p, p.UserID, doctorID, time.Duration(req.TTLHours)*time.Hour)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toShareResponse(share))
}

func (h *Handler) listShares(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	shares, err := h.access.ListShares(r.Context(), p, p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]shareResponse, len(shares))
	for i := range shares {
		out[i] = toShareResponse(shares[i])
	}
	httpx.OK(w, r, out)
}

func (h *Handler) revokeShare(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.access.RevokeShare(r.Context(), p, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

// --- response shapes ---------------------------------------------------

type documentResponse struct {
	ID              string `json:"id"`
	OwnerUserID     string `json:"owner_user_id"`
	UploadedBy      string `json:"uploaded_by"`
	DocumentType    string `json:"document_type"`
	Filename        string `json:"filename"`
	ContentType     string `json:"content_type"`
	SizeBytes       int64  `json:"size_bytes"`
	ChecksumSHA256  string `json:"checksum_sha256"`
	ScanStatus      string `json:"scan_status"`
	FHIRReferenceID string `json:"fhir_reference_id,omitempty"`
	CreatedAt       string `json:"created_at"`
}

func toDocumentResponse(d Document) documentResponse {
	return documentResponse{
		ID: d.ID.String(), OwnerUserID: d.OwnerUserID.String(), UploadedBy: d.UploadedBy.String(),
		DocumentType: string(d.DocumentType), Filename: d.Filename, ContentType: d.ContentType,
		SizeBytes: d.SizeBytes, ChecksumSHA256: d.ChecksumSHA256, ScanStatus: string(d.ScanStatus),
		FHIRReferenceID: d.FHIRReferenceID, CreatedAt: d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

type shareResponse struct {
	ID        string  `json:"id"`
	PatientID string  `json:"patient_id"`
	DoctorID  string  `json:"doctor_id"`
	GrantedBy string  `json:"granted_by"`
	ExpiresAt string  `json:"expires_at"`
	RevokedAt *string `json:"revoked_at,omitempty"`
	CreatedAt string  `json:"created_at"`
}

func toShareResponse(s access.RecordShare) shareResponse {
	out := shareResponse{
		ID: s.ID.String(), PatientID: s.PatientID.String(), DoctorID: s.DoctorID.String(),
		GrantedBy: s.GrantedBy.String(), ExpiresAt: s.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.RevokedAt != nil {
		v := s.RevokedAt.UTC().Format(time.RFC3339)
		out.RevokedAt = &v
	}
	return out
}
