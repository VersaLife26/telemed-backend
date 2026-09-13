// Package records owns the documents domain: patient-uploaded medical
// files (reports, scans, credentials, recordings) stored in MinIO and
// indexed here, with authorization delegated to internal/access.
package records

import (
	"time"

	"github.com/google/uuid"
)

// DocumentType is the kind of file a document row represents.
type DocumentType string

const (
	DocumentTypeReport       DocumentType = "report"
	DocumentTypeScan         DocumentType = "scan"
	DocumentTypePrescription DocumentType = "prescription"
	DocumentTypeCredential   DocumentType = "credential"
	DocumentTypeRecording    DocumentType = "recording"
)

// ValidDocumentTypes is used by request validation and the DB CHECK
// constraint's Go-side mirror.
var ValidDocumentTypes = map[DocumentType]bool{
	DocumentTypeReport: true, DocumentTypeScan: true, DocumentTypePrescription: true,
	DocumentTypeCredential: true, DocumentTypeRecording: true,
}

// BucketFor maps a document type to the bucket it is stored in. Credentials
// and recordings get their own buckets per AGENT-BRIEF; everything else
// lands in medical-reports.
func BucketFor(t DocumentType) string {
	switch t {
	case DocumentTypeCredential:
		return "doctor-credentials"
	case DocumentTypeRecording:
		return "recordings"
	default:
		return "medical-reports"
	}
}

// ScanStatus is the outcome of the virus scan pipeline (internal/platform/scan).
type ScanStatus string

const (
	ScanStatusPending  ScanStatus = "pending"
	ScanStatusClean    ScanStatus = "clean"
	ScanStatusInfected ScanStatus = "infected"
	ScanStatusSkipped  ScanStatus = "skipped"
)

// Document is one stored medical file.
type Document struct {
	ID              uuid.UUID
	OwnerUserID     uuid.UUID
	UploadedBy      uuid.UUID
	DocumentType    DocumentType
	Bucket          string
	ObjectKey       string
	Filename        string
	ContentType     string
	SizeBytes       int64
	ChecksumSHA256  string
	ScanStatus      ScanStatus
	FHIRReferenceID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
	Version         int
}

// ListFilter narrows a document listing.
type ListFilter struct {
	OwnerUserID  uuid.UUID
	DocumentType DocumentType // empty = any
	Page         int
	PerPage      int
}

// MaxUploadBytes is the hard cap on a single upload, enforced before a byte
// of the body is read into memory.
const MaxUploadBytes = 10 << 20 // 10MB

// allowedByExtension pairs each permitted file extension with the sniffed
// content types (via http.DetectContentType, never the client-supplied
// header) accepted for it. A file must pass both checks: its extension must
// be in this map, and what the server actually observes in the byte stream
// must be one of the listed types for that extension. This is deliberately
// narrower than a flat "allowed content types" set -- pairing them means a
// .pdf upload whose bytes sniff as anything other than a PDF is rejected,
// rather than waved through because *some* extension permits that type.
var allowedByExtension = map[string]map[string]bool{
	".pdf":  {"application/pdf": true},
	".jpg":  {"image/jpeg": true},
	".jpeg": {"image/jpeg": true},
	".png":  {"image/png": true},
	".webp": {"image/webp": true},
	".tif":  {"image/tiff": true},
	".tiff": {"image/tiff": true},
	".heic": {"image/heic": true, "application/octet-stream": true}, // HEIC often sniffs as octet-stream
	// DICOM carries a 128-byte preamble DetectContentType does not
	// recognise, so it always sniffs as octet-stream; the extension is the
	// only real signal for this format.
	".dcm":  {"application/octet-stream": true, "application/dicom": true},
	".mp4":  {"video/mp4": true},
	".webm": {"video/webm": true},
}

// IsAllowedUpload reports whether ext (lowercase, with leading dot) paired
// with sniffedContentType is a permitted upload.
func IsAllowedUpload(ext, sniffedContentType string) bool {
	allowed, ok := allowedByExtension[ext]
	if !ok {
		return false
	}
	return allowed[sniffedContentType]
}
