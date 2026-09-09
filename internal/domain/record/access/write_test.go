package access

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/middleware"
)

// Writing into somebody else's vault is a narrower right than reading it, and
// decideAccess used to answer only one question for both.
//
// POST /records/upload takes owner_user_id AND document_type from the request.
// document_type=credential maps to ResourceCredentialDocument, which every
// admin role is granted so the credentialing queue can open a doctor's SLMC
// certificate. That read grant was also a write grant: any of the five admin
// roles could plant a file in any person's medical-record index, or file a
// forged certificate under a real doctor's identity -- the same credential
// substitution F8 describes on the doctor-service side.
func TestUploadIntoAnotherVault_AdminMayReadCredentialsButNeverWrite(t *testing.T) {
	victim := uuid.New()

	for _, role := range middleware.AdminRoles {
		p := middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{role}}

		// The credentialing queue must keep working.
		if got := decideAccess(p, victim, ResourceCredentialDocument, ActionView, false, nil); !got.Granted {
			t.Fatalf("role %s can no longer READ a credential document (%s); the doctor-verification queue depends on it",
				role, got.Reason)
		}

		// But it may not write one.
		got := decideAccess(p, victim, ResourceCredentialDocument, ActionUpload, false, nil)
		if got.Granted {
			t.Fatalf("role %s uploaded a document into another user's vault as document_type=credential (reason %s)",
				role, got.Reason)
		}
		if got.Reason != ReasonDeniedWrite {
			t.Fatalf("role %s: reason = %s, want %s -- the refusal must be greppable as a read-only grant being used for a write",
				role, got.Reason, ReasonDeniedWrite)
		}

		// And nothing else either.
		for _, res := range []ResourceType{ResourceDocument, ResourcePrescription, ResourceClinicalNote} {
			if d := decideAccess(p, victim, res, ActionUpload, true, nil); d.Granted {
				t.Fatalf("role %s uploaded %s into a patient's vault", role, res)
			}
		}
	}
}

// A treating doctor legitimately files a lab result into their patient's
// vault. They do not file a CREDENTIAL there -- a credential is a doctor's own
// registration paperwork, kept in a different bucket, and a patient has none.
// The treating branch ignored the resource entirely, so this was free.
func TestUploadIntoAnotherVault_TreatingDoctorIsScopedToClinicalDocuments(t *testing.T) {
	doc := treatingDoctorPrincipal()

	if got := decideAccess(doc, patientID, ResourceDocument, ActionUpload, true, nil); !got.Granted {
		t.Fatalf("a treating doctor must still be able to file a lab result: %s", got.Reason)
	}

	for _, res := range []ResourceType{ResourceCredentialDocument, ResourcePrescription, ResourceClinicalNote} {
		got := decideAccess(doc, patientID, res, ActionUpload, true, nil)
		if got.Granted {
			t.Fatalf("a treating doctor uploaded %s into a patient's vault (reason %s)", res, got.Reason)
		}
	}
}

// A share is a patient handing a doctor a key to READ. Nothing in that gesture
// says "and you may add documents to my chart".
func TestUploadIntoAnotherVault_AShareIsReadOnly(t *testing.T) {
	share := &RecordShare{ID: uuid.New(), PatientID: patientID, DoctorID: doctorID, ExpiresAt: time.Now().Add(time.Hour)}
	doc := treatingDoctorPrincipal()

	if got := decideAccess(doc, patientID, ResourceDocument, ActionView, false, share); !got.Granted {
		t.Fatalf("a share must still grant a read: %s", got.Reason)
	}
	got := decideAccess(doc, patientID, ResourceDocument, ActionUpload, false, share)
	if got.Granted {
		t.Fatalf("a doctor wrote into a patient's vault on the strength of a read share (reason %s)", got.Reason)
	}
	if got.Reason != ReasonDeniedWrite {
		t.Fatalf("reason = %s, want %s", got.Reason, ReasonDeniedWrite)
	}
}

// The patient's own writes are untouched: they never reach decideAccess for
// their own vault, and where they do the owner branch answers first.
func TestUploadIntoOwnVaultIsStillAllowed(t *testing.T) {
	if got := decideAccess(patientPrincipal(), patientID, ResourceDocument, ActionUpload, false, nil); !got.Granted {
		t.Fatalf("a patient can no longer upload to their own vault: %s", got.Reason)
	}
}

// IsWrite must classify every action, so a new one added later cannot silently
// arrive on the read side of the fence.
func TestActionIsWriteCoversEveryAction(t *testing.T) {
	writes := map[Action]bool{
		ActionUpload: true, ActionFinalise: true, ActionAmend: true,
		ActionView: false, ActionDownload: false, ActionVerify: false, ActionList: false,
	}
	for a, want := range writes {
		if a.IsWrite() != want {
			t.Fatalf("Action(%q).IsWrite() = %v, want %v", a, a.IsWrite(), want)
		}
	}
}

// records.Delete destroys a patient's medical document. It used to be the only
// PHI-touching method in the service that called neither access.Check nor
// InsertAccessLog -- an inline role check, then SoftDelete -- so any of the
// five admin roles could erase a chart with no entry in the append-only log
// that exists to answer "who touched this record". The action CHECK constraint
// did not even permit 'delete', so it could not have been logged without
// migration 000008.
func TestDeleteIsAWriteAndAdminsCannotDoIt(t *testing.T) {
	victim := uuid.New()

	if !ActionDelete.IsWrite() {
		t.Fatal("deletion must be classified as a write, or the read-only admin grants reach it")
	}

	for _, role := range middleware.AdminRoles {
		p := middleware.Principal{UserID: otherUserID, Roles: []middleware.Role{role}}
		for _, res := range []ResourceType{ResourceDocument, ResourceCredentialDocument, ResourcePrescription} {
			got := decideAccess(p, victim, res, ActionDelete, true, nil)
			if got.Granted {
				t.Fatalf("role %s deleted a %s from another user's vault (reason %s)", role, res, got.Reason)
			}
		}
	}

	// A treating doctor does not delete a patient's documents either.
	if got := decideAccess(treatingDoctorPrincipal(), patientID, ResourceDocument, ActionDelete, true, nil); got.Granted {
		t.Fatal("a treating doctor deleted a document from their patient's vault")
	}

	// The patient still can.
	if got := decideAccess(patientPrincipal(), patientID, ResourceDocument, ActionDelete, false, nil); !got.Granted {
		t.Fatalf("a patient can no longer delete their own document: %s", got.Reason)
	}
}
