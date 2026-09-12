package doctor

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SECURITY-REVIEW F8.
//
// The exploit: Dr B registers, obtains Dr A's slmc_certificate_key (it is on
// the doctor.documents_updated event, in admin-service's projection, and in
// any log line that ever carried it), and POSTs it as their own SLMC
// certificate. The credentialing reviewer opens a presigned URL to A's genuine
// certificate and approves B. Someone gets approved to practise medicine on
// another doctor's credentials.
//
// The fix is that the key is derived from the caller's own doctor id, so there
// is no request in which one doctor's key can be attached to another doctor's
// application.

func TestDeriveCredentialKey_IsBoundToTheUploadersOwnDoctorID(t *testing.T) {
	t.Parallel()

	drA := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	drB := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")

	keyA := DeriveCredentialKey(drA, DocumentSLMCCertificate, "slmc.pdf")
	keyB := DeriveCredentialKey(drB, DocumentSLMCCertificate, "slmc.pdf")

	if !strings.HasPrefix(keyA, drA.String()+"/") {
		t.Fatalf("key %q is not prefixed with the uploading doctor's id %s", keyA, drA)
	}
	if strings.Contains(keyB, drA.String()) {
		t.Fatalf("Dr B's key %q contains Dr A's id: the prefix is not doing the one job it has", keyB)
	}
	if keyA == keyB {
		t.Fatal("two doctors derived the same key")
	}
}

// Every upload is a distinct object, so re-submitting a rejected certificate
// cannot overwrite the one a reviewer already looked at. Documents are
// append-only and the record of what was submitted before has to survive.
func TestDeriveCredentialKey_NeverCollidesWithAPreviousUpload(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	seen := make(map[string]struct{}, 512)
	for range 512 {
		k := DeriveCredentialKey(id, DocumentNIC, "nic.jpg")
		if _, dup := seen[k]; dup {
			t.Fatalf("derived a duplicate key %q: a re-upload would silently replace an earlier document", k)
		}
		seen[k] = struct{}{}
	}
}

// The filename is the only part of the key a client can influence at all, so
// it is an allowlist rather than a sanitiser. These are the inputs that would
// matter if it were the latter.
func TestDeriveCredentialKey_TakesNothingButAnAllowlistedSuffixFromTheFilename(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")

	cases := []struct {
		name     string
		filename string
		wantExt  string
	}{
		{"an ordinary pdf", "slmc.pdf", ".pdf"},
		{"uppercase extension", "SLMC.PDF", ".pdf"},
		{"a photo", "me.JPEG", ".jpeg"},
		{"no extension at all", "certificate", ""},
		{"empty filename", "", ""},
		{"traversal in the name", "../../../etc/passwd", ""},
		{"traversal with a dot segment", "../../a/../b", ""},
		{"an absolute path", "/etc/shadow", ""},
		{"a windows path", `C:\Users\admin\secret.exe`, ""},
		{"another doctor's key as the filename", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/slmc.pdf", ".pdf"},
		{"a disallowed executable suffix", "payload.sh", ""},
		{"a double extension", "invoice.pdf.exe", ""},
		// path.Ext reads from the FINAL dot, so the control characters live in
		// the stem and the stem is discarded. The suffix that survives is a
		// plain ".pdf".
		{"a null byte", "a\x00.pdf", ".pdf"},
		{"a newline", "a\n.pdf", ".pdf"},
		{"a slash inside the extension position", "a.b/c", ""},
		{"just a dot", ".", ""},
		{"a dotfile", ".bashrc", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := DeriveCredentialKey(id, DocumentSLMCCertificate, tc.filename)

			// Structure first: three segments, the first of which is the
			// caller's own id. Anything that escaped would change the count.
			parts := strings.Split(key, "/")
			if len(parts) != 3 {
				t.Fatalf("key %q has %d segments, want 3 (<doctor_id>/<type>/<uuid><ext>)", key, len(parts))
			}
			if parts[0] != id.String() {
				t.Fatalf("key %q does not start with the caller's doctor id", key)
			}
			if parts[1] != string(DocumentSLMCCertificate) {
				t.Fatalf("key %q has document type segment %q", key, parts[1])
			}

			// The last segment is a uuid plus at most an allowlisted suffix.
			last := parts[2]
			if !strings.HasSuffix(last, tc.wantExt) {
				t.Fatalf("key %q does not end with %q", key, tc.wantExt)
			}
			randomPart := strings.TrimSuffix(last, tc.wantExt)
			if _, err := uuid.Parse(randomPart); err != nil {
				t.Fatalf("the unique segment of %q is not a uuid: %v", key, err)
			}

			// The uuid.Parse above is already the complete guarantee that
			// nothing from the filename except the allowlisted suffix reached
			// the key: the last segment is exactly a uuid plus that suffix,
			// and there is nowhere else for client bytes to be. What is left
			// to check is that the escape attempts did not land anywhere.
			for _, bad := range []string{"..", "\x00", "\n", `\`, "etc/", "Users"} {
				if strings.Contains(key, bad) {
					t.Fatalf("key %q contains %q from filename %q", key, bad, tc.filename)
				}
			}
			// The case that is the actual exploit: another doctor's id offered
			// as the filename must not end up anywhere in this doctor's key.
			if strings.Contains(key, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") {
				t.Fatalf("another doctor's id reached the key %q via the filename", key)
			}
		})
	}
}

// The document type is a closed enum validated by the handler before it gets
// here, so it cannot inject a segment either. This pins that the key layout
// does not silently start depending on unvalidated input if that changes.
func TestDeriveCredentialKey_CoversEveryDocumentType(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	for _, dt := range []DocumentType{
		DocumentSLMCCertificate, DocumentNIC, DocumentDegreeCertificate,
		DocumentSpecialtyBoardCert, DocumentPhoto, DocumentOther,
		DocumentSignature, DocumentSeal,
	} {
		if !dt.Valid() {
			t.Fatalf("%q is not a valid document type", dt)
		}
		key := DeriveCredentialKey(id, dt, "x.pdf")
		if got := strings.Split(key, "/"); len(got) != 3 || got[1] != string(dt) {
			t.Fatalf("key %q for type %q has the wrong shape", key, dt)
		}
	}
}
