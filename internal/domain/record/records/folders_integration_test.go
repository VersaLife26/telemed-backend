//go:build integration

package records

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

func statusOf(err error) int {
	var apiErr *httpx.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status()
	}
	return 0
}

func uploadPDF(t *testing.T, svc *Service, p middleware.Principal, folder *uuid.UUID) Document {
	t.Helper()
	doc, err := svc.Upload(context.Background(), UploadInput{
		Principal: p, DocumentType: DocumentTypeReport, FolderID: folder,
		Filename: "report.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return doc
}

// A document or folder may only ever be placed in a folder of its own vault.
// This is the server-side guarantee that no file crosses between patients.
func TestFolders_MovesNeverLeaveTheVault(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()
	alice := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	bob := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}

	aliceFolder, err := svc.CreateFolder(ctx, alice, uuid.Nil, nil, "Labs", "", "")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	bobFolder, err := svc.CreateFolder(ctx, bob, uuid.Nil, nil, "Labs", "", "")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	doc := uploadPDF(t, svc, alice, nil)

	if _, err := svc.UpdateDocument(ctx, alice, doc.ID, nil, Placement{Set: true, ID: &bobFolder.ID}, "", ""); statusOf(err) != http.StatusNotFound {
		t.Fatalf("moving a document into another vault's folder: err = %v, want 404", err)
	}
	if _, err := svc.UpdateFolder(ctx, alice, aliceFolder.ID, nil, Placement{Set: true, ID: &bobFolder.ID}, "", ""); statusOf(err) != http.StatusNotFound {
		t.Fatalf("moving a folder into another vault's folder: err = %v, want 404", err)
	}
	if _, err := svc.Upload(ctx, UploadInput{
		Principal: alice, DocumentType: DocumentTypeReport, FolderID: &bobFolder.ID,
		Filename: "x.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	}); statusOf(err) != http.StatusNotFound {
		t.Fatalf("uploading into another vault's folder: err = %v, want 404", err)
	}

	moved, err := svc.UpdateDocument(ctx, alice, doc.ID, nil, Placement{Set: true, ID: &aliceFolder.ID}, "", "")
	if err != nil {
		t.Fatalf("move within the vault: %v", err)
	}
	if moved.FolderID == nil || *moved.FolderID != aliceFolder.ID {
		t.Fatalf("FolderID = %v, want %s", moved.FolderID, aliceFolder.ID)
	}
}

// A treating doctor may read and file into a vault, but reorganising or
// deleting what the patient keeps there is the patient's decision.
func TestFolders_TreatingDoctorCannotRenameMoveOrDelete(t *testing.T) {
	svc, accessSvc, accessRepo := newIntegrationService(t)
	ctx := context.Background()
	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	doctorID := uuid.New()
	doctor := middleware.Principal{UserID: uuid.New(), DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
	started := time.Now().Add(-time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), uuid.New(), doctorID, patient.UserID, &started, nil); err != nil {
		t.Fatalf("seed treating relationship: %v", err)
	}

	folder, err := svc.CreateFolder(ctx, doctor, patient.UserID, nil, "From Dr. X", "", "")
	if err != nil {
		t.Fatalf("treating doctor CreateFolder: %v", err)
	}
	listing, err := svc.ListFolders(ctx, doctor, patient.UserID, nil, "", "")
	if err != nil || len(listing.Folders) != 1 {
		t.Fatalf("treating doctor ListFolders = %v, %v", listing.Folders, err)
	}
	doc := uploadPDF(t, svc, patient, nil)

	name := "renamed"
	if _, err := svc.UpdateFolder(ctx, doctor, folder.ID, &name, Placement{}, "", ""); statusOf(err) != http.StatusForbidden {
		t.Errorf("doctor rename folder: err = %v, want 403", err)
	}
	if _, err := svc.UpdateDocument(ctx, doctor, doc.ID, nil, Placement{Set: true, ID: &folder.ID}, "", ""); statusOf(err) != http.StatusForbidden {
		t.Errorf("doctor move document: err = %v, want 403", err)
	}
	if err := svc.DeleteFolder(ctx, doctor, folder.ID, "", ""); statusOf(err) != http.StatusForbidden {
		t.Errorf("doctor delete folder: err = %v, want 403", err)
	}

	patients, err := svc.Patients(ctx, doctor)
	if err != nil || len(patients) != 1 || patients[0].UserID != patient.UserID {
		t.Errorf("Patients = %v, %v; want the one treated patient", patients, err)
	}
}

func TestFolders_CycleEmptinessAndSiblingNames(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()
	p := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}

	parent, err := svc.CreateFolder(ctx, p, uuid.Nil, nil, "2026", "", "")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	child, err := svc.CreateFolder(ctx, p, uuid.Nil, &parent.ID, "March", "", "")
	if err != nil {
		t.Fatalf("CreateFolder child: %v", err)
	}
	if _, err := svc.CreateFolder(ctx, p, uuid.Nil, nil, "2026", "", ""); statusOf(err) != http.StatusConflict {
		t.Errorf("duplicate sibling name: err = %v, want 409", err)
	}
	if _, err := svc.UpdateFolder(ctx, p, parent.ID, nil, Placement{Set: true, ID: &child.ID}, "", ""); statusOf(err) != http.StatusUnprocessableEntity {
		t.Errorf("moving a folder into its own child: err = %v, want 422", err)
	}
	if err := svc.DeleteFolder(ctx, p, parent.ID, "", ""); statusOf(err) != http.StatusConflict {
		t.Errorf("deleting a non-empty folder: err = %v, want 409", err)
	}

	listing, err := svc.ListFolders(ctx, p, uuid.Nil, &child.ID, "", "")
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(listing.Path) != 2 || listing.Path[0].ID != parent.ID || listing.Path[1].ID != child.ID {
		t.Errorf("Path = %v, want [2026, March]", listing.Path)
	}

	if err := svc.DeleteFolder(ctx, p, child.ID, "", ""); err != nil {
		t.Fatalf("delete empty child: %v", err)
	}
	if err := svc.DeleteFolder(ctx, p, parent.ID, "", ""); err != nil {
		t.Fatalf("delete now-empty parent: %v", err)
	}
}
