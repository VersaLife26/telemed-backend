package records

import "testing"

// TestIsAllowedUpload_RejectionCases exercises the extension +
// http.DetectContentType pairing that gates every upload: AGENT-BRIEF
// requires trusting the sniffed content type, never the client's declared
// one, and an independent extension allowlist on top of it.
func TestIsAllowedUpload_RejectionCases(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		sniffed string
		want    bool
	}{
		{"pdf with matching sniff", ".pdf", "application/pdf", true},
		{"jpg with matching sniff", ".jpg", "image/jpeg", true},
		{"jpeg with matching sniff", ".jpeg", "image/jpeg", true},
		{"png with matching sniff", ".png", "image/png", true},
		{"webp with matching sniff", ".webp", "image/webp", true},
		{"tiff with matching sniff", ".tiff", "image/tiff", true},
		{"tif with matching sniff", ".tif", "image/tiff", true},
		{"mp4 with matching sniff", ".mp4", "video/mp4", true},
		{"webm with matching sniff", ".webm", "video/webm", true},
		{"dicom sniffs as octet-stream", ".dcm", "application/octet-stream", true},
		{"ID3-tagged mp3", ".mp3", "audio/mpeg", true},
		{"untagged mp3 sniffs as octet-stream", ".mp3", "application/octet-stream", true},
		{"m4a sniffs as video/mp4", ".m4a", "video/mp4", true},
		{"wav with matching sniff", ".wav", "audio/wave", true},
		{"ogg with matching sniff", ".ogg", "application/ogg", true},
		{"html renamed to .mp3 must be rejected", ".mp3", "text/html", false},
		{"pdf renamed to .wav must be rejected", ".wav", "application/pdf", false},

		{
			name: "an executable renamed to .pdf must be rejected",
			// A Windows PE binary or ELF renamed to report.pdf: the
			// extension passes a naive check, but DetectContentType sees
			// the real bytes.
			ext: ".pdf", sniffed: "application/octet-stream", want: false,
		},
		{
			name: "an HTML file (XSS payload) renamed to .jpg must be rejected",
			ext:  ".jpg", sniffed: "text/html", want: false,
		},
		{
			name: "a script renamed with a permitted extension must be rejected",
			ext:  ".png", sniffed: "text/x-shellscript", want: false,
		},
		{
			name: "extension not in the allowlist at all",
			ext:  ".exe", sniffed: "application/pdf", want: false,
		},
		{
			name: "no extension",
			ext:  "", sniffed: "application/pdf", want: false,
		},
		{
			name: "a pdf whose bytes are actually a zip (polyglot) must be rejected",
			ext:  ".pdf", sniffed: "application/zip", want: false,
		},
		{
			name: "jpg content served with png extension must be rejected",
			ext:  ".jpg", sniffed: "image/png", want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAllowedUpload(tc.ext, tc.sniffed); got != tc.want {
				t.Errorf("IsAllowedUpload(%q, %q) = %v, want %v", tc.ext, tc.sniffed, got, tc.want)
			}
		})
	}
}

func TestBucketFor(t *testing.T) {
	tests := []struct {
		docType DocumentType
		want    string
	}{
		{DocumentTypeReport, "medical-reports"},
		{DocumentTypeScan, "medical-reports"},
		{DocumentTypePrescription, "medical-reports"},
		{DocumentTypeCredential, "doctor-credentials"},
		{DocumentTypeRecording, "recordings"},
	}
	for _, tc := range tests {
		if got := BucketFor(tc.docType); got != tc.want {
			t.Errorf("BucketFor(%s) = %s, want %s", tc.docType, got, tc.want)
		}
	}
}

func TestValidDocumentTypes(t *testing.T) {
	for dt := range ValidDocumentTypes {
		if !ValidDocumentTypes[dt] {
			t.Errorf("expected %s to be valid", dt)
		}
	}
	if ValidDocumentTypes[DocumentType("not-a-real-type")] {
		t.Error("expected an unknown document type to be invalid")
	}
}
