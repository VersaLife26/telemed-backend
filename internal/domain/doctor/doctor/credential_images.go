package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register the decoder image.DecodeConfig needs
	_ "image/png"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
	"telemed/internal/platform/storage"
)

// MaxCredentialImageBytes caps a signature or seal upload.
const MaxCredentialImageBytes = 1 << 20 // 1 MiB

// CredentialImage is a stored signature or seal, read back for its owner.
type CredentialImage struct {
	Data        []byte
	ContentType string
}

// WithCredentialStore attaches the object store signature and seal images are
// written to. Nil leaves those endpoints answering 503 and makes Attach refuse
// an application that carries a signature or seal it cannot copy.
func (s *Service) WithCredentialStore(store storage.Storage) *Service {
	s.store = store
	return s
}

// credentialImageExt returns ".png" or ".jpg" for bytes that actually decode
// as that format, or "" for anything else. The format comes from the decoder,
// never from a filename or Content-Type header: record-service's prescription
// renderer picks its fpdf image type from the key's extension, so a mislabelled
// key would fail the whole PDF rather than just this image.
func credentialImageExt(data []byte) string {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	switch format {
	case "png":
		return ".png"
	case "jpeg":
		return ".jpg"
	}
	return ""
}

// credentialDocumentKind sniffs an uploaded credential document. The extension
// comes from the bytes, never the client's filename, so a reviewer's download
// opens with the right application.
func credentialDocumentKind(data []byte) (ext, contentType string) {
	switch ct := http.DetectContentType(data); ct {
	case "application/pdf":
		return ".pdf", ct
	case "image/jpeg":
		return ".jpg", ct
	case "image/png":
		return ".png", ct
	case "image/webp":
		return ".webp", ct
	}
	return "", ""
}

func credentialImageContentType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	}
	return "application/octet-stream"
}

// signatureSealKey derives the storage key; see objectkey.go for why keys are
// never client-supplied.
func signatureSealKey(doctorID uuid.UUID, docType DocumentType, ext string) string {
	return fmt.Sprintf("doctors/%s/%s-%s%s", doctorID, docType, uuid.New(), ext)
}

// putCredentialImage validates and writes the bytes, returning the unsaved
// document row for them.
func (s *Service) putCredentialImage(ctx context.Context, doctorID uuid.UUID, docType DocumentType, data []byte) (*Document, error) {
	if s.store == nil {
		return nil, ErrCredentialStoreUnavailable
	}
	if len(data) > MaxCredentialImageBytes {
		return nil, ErrCredentialImageTooLarge
	}
	ext := credentialImageExt(data)
	if ext == "" {
		return nil, ErrInvalidCredentialImage
	}
	key := signatureSealKey(doctorID, docType, ext)
	if err := s.store.Put(ctx, storage.BucketDoctorCredentials, key, bytes.NewReader(data), int64(len(data)), credentialImageContentType(key)); err != nil {
		return nil, fmt.Errorf("doctor: store %s image: %w", docType, err)
	}
	return &Document{ID: uuid.New(), DoctorID: doctorID, DocumentType: docType, ObjectKey: key, UploadedAt: time.Now().UTC()}, nil
}

// SetCredentialImage stores the calling doctor's signature or seal. Earlier
// uploads are kept: documents are append-only and the newest row wins (see
// SignatureAndSealKeys).
//
// No doctor.documents_updated event is enqueued: that payload carries only the
// four admin-review keys, none of which a signature or seal changes.
func (s *Service) SetCredentialImage(ctx context.Context, userID uuid.UUID, docType DocumentType, data []byte) (Document, error) {
	if docType != DocumentSignature && docType != DocumentSeal {
		return Document{}, ErrInvalidDocumentType
	}
	d, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return Document{}, err
	}
	doc, err := s.putCredentialImage(ctx, d.ID, docType, data)
	if err != nil {
		return Document{}, err
	}
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return s.repo.InsertDocument(ctx, tx, doc)
	})
	if err != nil {
		return Document{}, err
	}
	return *doc, nil
}

// GetCredentialImage returns the calling doctor's newest signature or seal.
func (s *Service) GetCredentialImage(ctx context.Context, userID uuid.UUID, docType DocumentType) (CredentialImage, error) {
	if s.store == nil {
		return CredentialImage{}, ErrCredentialStoreUnavailable
	}
	d, err := s.repo.GetByUserID(ctx, userID)
	if err != nil {
		return CredentialImage{}, err
	}
	docs, err := s.repo.ListDocuments(ctx, d.ID)
	if err != nil {
		return CredentialImage{}, err
	}
	signatureKey, sealKey := SignatureAndSealKeys(docs)
	key := signatureKey
	if docType == DocumentSeal {
		key = sealKey
	}
	if key == "" {
		return CredentialImage{}, ErrNotFound
	}
	rc, err := s.store.Get(ctx, storage.BucketDoctorCredentials, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return CredentialImage{}, ErrNotFound
		}
		return CredentialImage{}, fmt.Errorf("doctor: fetch %s image: %w", docType, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return CredentialImage{}, fmt.Errorf("doctor: read %s image: %w", docType, err)
	}
	return CredentialImage{Data: data, ContentType: credentialImageContentType(key)}, nil
}

// applicationCredentialImages copies the signature and seal a public
// application collected into the bucket, returning the document rows for the
// caller to insert in the same transaction that creates the doctors row
// (doctor_documents has a foreign key to it).
//
// A storage failure is returned, failing the attach: it is retried (by the
// user-service consumer or the next approve), and a doctor activated without
// the signature they already submitted cannot issue a prescription. An image
// that is not a decodable PNG/JPEG is skipped with a warning instead, because
// no retry can fix it -- the doctor re-uploads it from their profile.
func (s *Service) applicationCredentialImages(ctx context.Context, app Application) ([]*Document, error) {
	appDocs, err := s.repo.ListApplicationDocuments(ctx, app.ID, true)
	if err != nil {
		return nil, err
	}
	var out []*Document
	for i := range appDocs {
		ad := &appDocs[i]
		if ad.DocumentType != DocumentSignature && ad.DocumentType != DocumentSeal {
			continue
		}
		doc, err := s.putCredentialImage(ctx, app.ID, ad.DocumentType, ad.Bytes)
		if errors.Is(err, ErrInvalidCredentialImage) || errors.Is(err, ErrCredentialImageTooLarge) {
			s.log.Warn().Str("application_id", app.ID.String()).Str("document_type", string(ad.DocumentType)).
				Msg("application credential image is not a png/jpeg within the size limit; not copied to the doctor profile")
			continue
		}
		if err != nil {
			s.log.Error().Err(err).Str("application_id", app.ID.String()).Str("document_type", string(ad.DocumentType)).
				Msg("failed to copy application credential image to the doctor profile")
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}
