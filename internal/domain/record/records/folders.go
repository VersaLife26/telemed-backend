package records

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"telemed/internal/domain/record/access"
	"telemed/internal/platform/database"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// NameResolver turns user ids into display names. It is optional: without
// one, the patient list carries ids only and the client shows a placeholder.
type NameResolver interface {
	Names(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]string
}

// SetNameResolver attaches the resolver used by Patients.
func (s *Service) SetNameResolver(n NameResolver) { s.names = n }

// Placement is an optional move target. Set false means "leave it where it
// is"; Set true with ID nil means "the vault root".
type Placement struct {
	Set bool
	ID  *uuid.UUID
}

var (
	errFolderNotFound = httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, "folder not found")
	errFolderName     = httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "folder name must be 1-120 characters and contain no slashes")
	errFolderCycle    = httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "a folder cannot be moved inside itself")
	errFolderNotEmpty = httpx.NewError(http.StatusConflict, httpx.CodeConflict, "only an empty folder can be deleted")
	errDuplicateName  = httpx.NewError(http.StatusConflict, httpx.CodeConflict, "a folder with this name already exists here")
	errFilename       = httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "file name must be 1-255 characters and contain no slashes")
)

func cleanFolderName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || len([]rune(name)) > MaxFolderNameLength || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", errFolderName
	}
	return name, nil
}

// cleanFilename validates a rename. The extension is fixed at upload time --
// it is half of the allowlist check -- so a rename keeps it whatever the
// caller typed.
func cleanFilename(raw, original string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", errFilename
	}
	ext := filepath.Ext(original)
	if !strings.EqualFold(filepath.Ext(name), ext) {
		name += ext
	}
	if len([]rune(name)) > 255 {
		return "", errFilename
	}
	return name, nil
}

func mapFolderWriteErr(err error) error {
	switch {
	case errors.Is(err, ErrDuplicateFolderName):
		return errDuplicateName
	case errors.Is(err, database.ErrOptimisticLock):
		return httpx.ErrConflict.WithCause(err)
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

// folderInVault loads a folder and confirms it belongs to ownerUserID. A
// folder from another vault is reported as not found, never as forbidden,
// so a caller cannot probe for folder ids outside the vault they hold.
func (s *Service) folderInVault(ctx context.Context, id, ownerUserID uuid.UUID) (Folder, error) {
	f, ok, err := s.repo.GetFolder(ctx, s.pool, id)
	if err != nil {
		return Folder{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok || f.OwnerUserID != ownerUserID {
		return Folder{}, errFolderNotFound
	}
	return f, nil
}

func (s *Service) checkVault(ctx context.Context, caller middleware.Principal, ownerUserID, resourceID uuid.UUID, action access.Action, ip, ua string) error {
	return s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: ownerUserID, Resource: access.ResourceDocument,
		ResourceID: resourceID, Action: action, IPAddress: ip, UserAgent: ua,
	})
}

// FolderListing is one level of a vault: its child folders and the chain of
// folders leading to it (empty at the root).
type FolderListing struct {
	Folders []Folder
	Path    []Folder
}

// ListFolders lists the child folders of parentID (nil = root) in
// ownerUserID's vault.
func (s *Service) ListFolders(ctx context.Context, caller middleware.Principal, ownerUserID uuid.UUID, parentID *uuid.UUID, ip, ua string) (FolderListing, error) {
	if ownerUserID == uuid.Nil {
		ownerUserID = caller.UserID
	}
	if ownerUserID != caller.UserID {
		if err := s.checkVault(ctx, caller, ownerUserID, ownerUserID, access.ActionList, ip, ua); err != nil {
			return FolderListing{}, err
		}
	}
	var out FolderListing
	if parentID != nil {
		if _, err := s.folderInVault(ctx, *parentID, ownerUserID); err != nil {
			return FolderListing{}, err
		}
		path, err := s.repo.FolderPath(ctx, s.pool, *parentID)
		if err != nil {
			return FolderListing{}, httpx.ErrInternal.WithCause(err)
		}
		out.Path = path
	}
	folders, err := s.repo.ListFolders(ctx, s.pool, ownerUserID, parentID)
	if err != nil {
		return FolderListing{}, httpx.ErrInternal.WithCause(err)
	}
	out.Folders = folders
	return out, nil
}

// CreateFolder makes a folder. A treating doctor may create one in their
// patient's vault, under the same grant that lets them upload into it.
func (s *Service) CreateFolder(ctx context.Context, caller middleware.Principal, ownerUserID uuid.UUID, parentID *uuid.UUID, rawName, ip, ua string) (Folder, error) {
	if ownerUserID == uuid.Nil {
		ownerUserID = caller.UserID
	}
	name, err := cleanFolderName(rawName)
	if err != nil {
		return Folder{}, err
	}
	id := uuid.New()
	if ownerUserID != caller.UserID {
		if err := s.checkVault(ctx, caller, ownerUserID, id, access.ActionUpload, ip, ua); err != nil {
			return Folder{}, err
		}
	}
	if parentID != nil {
		if _, err := s.folderInVault(ctx, *parentID, ownerUserID); err != nil {
			return Folder{}, err
		}
	}
	f, err := s.repo.CreateFolder(ctx, s.pool, Folder{
		ID: id, OwnerUserID: ownerUserID, ParentID: parentID, Name: name, CreatedBy: caller.UserID,
	})
	if err != nil {
		return Folder{}, mapFolderWriteErr(err)
	}
	return f, nil
}

// UpdateFolder renames and/or moves a folder within its own vault. It is an
// amend, which only the vault owner holds.
func (s *Service) UpdateFolder(ctx context.Context, caller middleware.Principal, id uuid.UUID, rawName *string, move Placement, ip, ua string) (Folder, error) {
	f, ok, err := s.repo.GetFolder(ctx, s.pool, id)
	if err != nil {
		return Folder{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Folder{}, errFolderNotFound
	}
	if err := s.checkVault(ctx, caller, f.OwnerUserID, f.ID, access.ActionAmend, ip, ua); err != nil {
		return Folder{}, err
	}

	name := f.Name
	if rawName != nil {
		if name, err = cleanFolderName(*rawName); err != nil {
			return Folder{}, err
		}
	}
	parent := f.ParentID
	if move.Set {
		parent = move.ID
		if parent != nil {
			if _, err := s.folderInVault(ctx, *parent, f.OwnerUserID); err != nil {
				return Folder{}, err
			}
			chain, err := s.repo.FolderPath(ctx, s.pool, *parent)
			if err != nil {
				return Folder{}, httpx.ErrInternal.WithCause(err)
			}
			for _, c := range chain {
				if c.ID == f.ID {
					return Folder{}, errFolderCycle
				}
			}
		}
	}

	out, err := s.repo.UpdateFolder(ctx, s.pool, f.ID, name, parent, f.Version)
	if err != nil {
		return Folder{}, mapFolderWriteErr(err)
	}
	return out, nil
}

// DeleteFolder removes an empty folder.
func (s *Service) DeleteFolder(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) error {
	f, ok, err := s.repo.GetFolder(ctx, s.pool, id)
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return errFolderNotFound
	}
	if err := s.checkVault(ctx, caller, f.OwnerUserID, f.ID, access.ActionDelete, ip, ua); err != nil {
		return err
	}
	empty, err := s.repo.FolderIsEmpty(ctx, s.pool, f.ID)
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}
	if !empty {
		return errFolderNotEmpty
	}
	if err := s.repo.SoftDeleteFolder(ctx, s.pool, f.ID, f.Version); err != nil {
		return mapFolderWriteErr(err)
	}
	return nil
}

// UpdateDocument renames a document and/or files it into another folder of
// the SAME vault. The same-owner check on the target folder is what keeps a
// file from ever crossing between two patients' records.
func (s *Service) UpdateDocument(ctx context.Context, caller middleware.Principal, id uuid.UUID, rawName *string, move Placement, ip, ua string) (Document, error) {
	doc, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return Document{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Document{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: doc.OwnerUserID, Resource: accessResourceFor(doc.DocumentType),
		ResourceID: doc.ID, Action: access.ActionAmend, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return Document{}, err
	}

	name := doc.Filename
	if rawName != nil {
		if name, err = cleanFilename(*rawName, doc.Filename); err != nil {
			return Document{}, err
		}
	}
	folder := doc.FolderID
	if move.Set {
		folder = move.ID
		if folder != nil {
			if _, err := s.folderInVault(ctx, *folder, doc.OwnerUserID); err != nil {
				return Document{}, err
			}
		}
	}

	out, err := s.repo.UpdateDocumentPlacement(ctx, s.pool, doc.ID, name, folder, doc.Version)
	if err != nil {
		if errors.Is(err, database.ErrOptimisticLock) {
			return Document{}, httpx.ErrConflict.WithCause(err)
		}
		return Document{}, httpx.ErrInternal.WithCause(err)
	}
	return out, nil
}

// Content authorizes like Download and opens the stored bytes, so a client
// can preview a file from its own origin rather than following a presigned
// URL to a storage host that serves everything as an attachment.
func (s *Service) Content(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) (io.ReadCloser, Document, error) {
	doc, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return nil, Document{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return nil, Document{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: doc.OwnerUserID, Resource: accessResourceFor(doc.DocumentType),
		ResourceID: doc.ID, Action: access.ActionDownload, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return nil, Document{}, err
	}
	if doc.ScanStatus == ScanStatusInfected {
		return nil, Document{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "this file was flagged by the virus scanner and cannot be downloaded")
	}
	body, err := s.store.Get(ctx, doc.Bucket, doc.ObjectKey)
	if err != nil {
		return nil, Document{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
			"document storage is unavailable").WithCause(err)
	}
	return body, doc, nil
}

// PatientRef is one vault a doctor can currently open.
type PatientRef struct {
	UserID uuid.UUID
	Name   string
}

// Patients lists the vaults the calling doctor can currently read, by name.
func (s *Service) Patients(ctx context.Context, caller middleware.Principal) ([]PatientRef, error) {
	if !caller.HasRole(middleware.RoleDoctor) || caller.DoctorID == uuid.Nil {
		return nil, httpx.ErrForbidden
	}
	ids, err := s.access.ReachablePatients(ctx, caller)
	if err != nil {
		return nil, err
	}
	var names map[uuid.UUID]string
	if s.names != nil && len(ids) > 0 {
		names = s.names.Names(ctx, ids)
	}
	out := make([]PatientRef, len(ids))
	for i, id := range ids {
		out[i] = PatientRef{UserID: id, Name: names[id]}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}
