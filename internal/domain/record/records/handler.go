package records

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
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
	r.Get("/patients", h.patients)
	r.Get("/folders", h.listFolders)
	r.Post("/folders", h.createFolder)
	r.Patch("/folders/{id}", h.updateFolder)
	r.Delete("/folders/{id}", h.deleteFolder)
	// Document ids are UUIDs. Without the regexp, GET /records/folders is
	// captured as GET /records/{id} with id="folders" and the client sees
	// "id must be a valid UUID" instead of the folder listing.
	const docID = "/{id:[0-9a-fA-F-]{36}}"
	r.Get(docID, h.get)
	r.Patch(docID, h.updateDocument)
	r.Get(docID+"/download", h.download)
	r.Get(docID+"/content", h.content)
	r.Delete(docID, h.delete)
	r.Post(docID+"/share", h.share)
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

	var folderID *uuid.UUID
	if raw := r.FormValue("folder_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "folder_id must be a valid UUID"))
			return
		}
		folderID = &id
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "multipart field \"file\" is required").WithCause(err))
		return
	}
	defer closeMultipart(file)

	filename := header.Filename
	if filename != "" {
		filename = strings.ReplaceAll(filename, "\\", "/")
		filename = filename[strings.LastIndex(filename, "/")+1:]
	}

	doc, err := h.svc.Upload(r.Context(), UploadInput{
		Principal: p, OwnerUserID: ownerID, FolderID: folderID, DocumentType: docType,
		Filename: filename, Data: file,
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
	folder, err := folderScopeParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, perPage, _ := httpx.Pagination(r)

	docs, total, err := h.svc.List(r.Context(), p, ownerID, docType, folder, page, perPage, middleware.ClientIP(r), r.UserAgent())
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
	url, doc, err := h.svc.Download(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent(), strings.EqualFold(r.URL.Query().Get("disposition"), "attachment"))
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

// folderScopeParam reads ?folder_id=: absent lists every folder, "root" the
// vault root, and a UUID that one folder.
func folderScopeParam(r *http.Request) (FolderScope, error) {
	raw := r.URL.Query().Get("folder_id")
	switch raw {
	case "":
		return FolderScope{}, nil
	case "root":
		return FolderScope{Set: true}, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return FolderScope{}, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "folder_id must be \"root\" or a valid UUID")
	}
	return FolderScope{Set: true, ID: &id}, nil
}

// optionalUUID parses a JSON placement field: nil leaves the item where it
// is, "" means the vault root, anything else must be a folder id.
func optionalUUID(raw *string, field string) (Placement, error) {
	if raw == nil {
		return Placement{}, nil
	}
	if *raw == "" {
		return Placement{Set: true}, nil
	}
	id, err := uuid.Parse(*raw)
	if err != nil {
		return Placement{}, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, field+" must be empty or a valid UUID")
	}
	return Placement{Set: true, ID: &id}, nil
}

func (h *Handler) patients(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	refs, err := h.svc.Patients(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]patientResponse, len(refs))
	for i, ref := range refs {
		out[i] = patientResponse{UserID: ref.UserID.String(), Name: ref.Name}
	}
	httpx.OK(w, r, out)
}

func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	ownerID := p.UserID
	if raw, ok, err := httpx.QueryUUID(r, "owner_user_id"); err != nil {
		httpx.Error(w, r, err)
		return
	} else if ok {
		ownerID = raw
	}
	var parentID *uuid.UUID
	if raw, ok, err := httpx.QueryUUID(r, "parent_id"); err != nil {
		httpx.Error(w, r, err)
		return
	} else if ok {
		parentID = &raw
	}
	listing, err := h.svc.ListFolders(r.Context(), p, ownerID, parentID, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := folderListingResponse{Folders: make([]folderResponse, len(listing.Folders)), Path: make([]folderResponse, len(listing.Path))}
	for i := range listing.Folders {
		out.Folders[i] = toFolderResponse(listing.Folders[i])
	}
	for i := range listing.Path {
		out.Path[i] = toFolderResponse(listing.Path[i])
	}
	httpx.OK(w, r, out)
}

type createFolderRequest struct {
	Name        string  `json:"name" validate:"required"`
	ParentID    *string `json:"parent_id"`
	OwnerUserID *string `json:"owner_user_id"`
}

func (h *Handler) createFolder(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	var req createFolderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	parent, err := optionalUUID(req.ParentID, "parent_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var ownerID uuid.UUID
	if req.OwnerUserID != nil && *req.OwnerUserID != "" {
		if ownerID, err = uuid.Parse(*req.OwnerUserID); err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "owner_user_id must be a valid UUID"))
			return
		}
	}
	f, err := h.svc.CreateFolder(r.Context(), p, ownerID, parent.ID, req.Name, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toFolderResponse(f))
}

type updateFolderRequest struct {
	Name     *string `json:"name"`
	ParentID *string `json:"parent_id"`
}

func (h *Handler) updateFolder(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req updateFolderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	move, err := optionalUUID(req.ParentID, "parent_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f, err := h.svc.UpdateFolder(r.Context(), p, id, req.Name, move, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toFolderResponse(f))
}

func (h *Handler) deleteFolder(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.DeleteFolder(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

type updateDocumentRequest struct {
	Filename *string `json:"filename"`
	FolderID *string `json:"folder_id"`
}

func (h *Handler) updateDocument(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req updateDocumentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	move, err := optionalUUID(req.FolderID, "folder_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doc, err := h.svc.UpdateDocument(r.Context(), p, id, req.Filename, move, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDocumentResponse(doc))
}

// content streams a document's bytes for in-app preview.
//
// Everything that reaches storage passed the extension/sniff allowlist, which
// admits no HTML, SVG or script, and the sandbox CSP below means that even a
// file that slipped through would render with no script and an opaque
// origin.
func (h *Handler) content(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	body, doc, err := h.svc.Content(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", PreviewContentType(doc.ContentType, doc.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(doc.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": doc.Filename}))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
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
	FolderID        string `json:"folder_id,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type folderResponse struct {
	ID          string `json:"id"`
	OwnerUserID string `json:"owner_user_id"`
	ParentID    string `json:"parent_id,omitempty"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toFolderResponse(f Folder) folderResponse {
	out := folderResponse{
		ID: f.ID.String(), OwnerUserID: f.OwnerUserID.String(), Name: f.Name,
		CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if f.ParentID != nil {
		out.ParentID = f.ParentID.String()
	}
	return out
}

type folderListingResponse struct {
	Folders []folderResponse `json:"folders"`
	Path    []folderResponse `json:"path"`
}

type patientResponse struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
}

func toDocumentResponse(d Document) documentResponse {
	out := documentResponse{
		ID: d.ID.String(), OwnerUserID: d.OwnerUserID.String(), UploadedBy: d.UploadedBy.String(),
		DocumentType: string(d.DocumentType), Filename: d.Filename, ContentType: d.ContentType,
		SizeBytes: d.SizeBytes, ChecksumSHA256: d.ChecksumSHA256, ScanStatus: string(d.ScanStatus),
		FHIRReferenceID: d.FHIRReferenceID, CreatedAt: d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if d.FolderID != nil {
		out.FolderID = d.FolderID.String()
	}
	return out
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
