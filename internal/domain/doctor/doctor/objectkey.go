package doctor

import (
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
)

// Storage keys for credential documents.
//
// # The finding this replaces
//
// POST /doctors/me/documents used to take `object_key` from the request body
// and store it verbatim. No prefix binding to the doctor, no ownership check,
// no traversal filter. admin-service later presigns whatever string is in that
// column against the `doctor-credentials` bucket, with no check that the key
// belongs to the doctor whose application is being reviewed.
//
// So Dr B, having obtained Dr A's slmc_certificate_key, could POST it as their
// own. The credentialing reviewer opens a presigned URL, sees A's genuine SLMC
// certificate, and approves B. That is credential substitution against a
// MEDICAL REGISTRATION workflow -- someone gets approved to practise medicine
// on another doctor's credentials.
//
// # Why deriving beats validating
//
// The obvious fix is to reject any client-supplied key not prefixed with the
// caller's own doctor id. That closes the reported exploit, and it leaves the
// client naming a path inside a PHI bucket -- so every future question ("can a
// key collide with an existing one?", "can it escape the prefix?", "what if
// the prefix is a prefix of another id?") stays live forever, and each one is
// answered by a regular expression somebody has to keep correct.
//
// Deriving the key removes the parameter instead of guarding it. There is no
// input to validate, no collision to reason about (uuid.New per upload), and
// the ownership prefix is a fact rather than an assertion. This is what
// record-service already does at records/service.go:157.

// CredentialBucket is the MinIO bucket credential documents live in. It is
// named here so the derived key's namespace is documented next to the key
// itself; this service never talks to MinIO, and admin-service presigns
// against the same bucket name.
const CredentialBucket = "doctor-credentials"

// allowedCredentialExtensions is the closed set of file suffixes that may
// appear in a derived key.
//
// The extension is cosmetic -- it decides what a browser calls the file when a
// reviewer downloads it -- but it is the ONLY part of the key with any client
// influence at all, so it is an allowlist rather than a sanitiser. An
// allowlist has no bypass to find.
var allowedCredentialExtensions = map[string]bool{
	".pdf":  true,
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".webp": true,
	".heic": true,
	".tif":  true,
	".tiff": true,
}

// DeriveCredentialKey returns the storage key for one credential upload:
//
//	<doctor_id>/<document_type>/<random uuid><ext>
//
// The doctor id prefix is what makes cross-doctor substitution structurally
// impossible: a key is derived from the authenticated caller's own profile, so
// there is no request in which one doctor's key can be attached to another
// doctor's application. The uuid makes every upload a distinct object, so
// re-submitting a rejected certificate never overwrites the one a reviewer
// already looked at -- documents are append-only and the audit trail of what
// was submitted before survives.
//
// filename is advisory and only ever contributes a suffix. path.Ext reads the
// final dot in the final slash-separated element, so it cannot return a value
// containing a separator, and the allowlist above means it cannot return an
// unexpected one either. An unrecognised or absent extension yields a key with
// none, which is correct rather than a failure: the object still stores and
// reviews fine.
func DeriveCredentialKey(doctorID uuid.UUID, docType DocumentType, filename string) string {
	return fmt.Sprintf("%s/%s/%s%s", doctorID, docType, uuid.New(), credentialExtension(filename))
}

func credentialExtension(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	if !allowedCredentialExtensions[ext] {
		return ""
	}
	return ext
}
