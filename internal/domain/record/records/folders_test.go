package records

import "testing"

func TestCleanFolderName(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Lab results", "Lab results", false},
		{"  padded  ", "padded", false},
		{"", "", true},
		{"   ", "", true},
		{"a/b", "", true},
		{`a\b`, "", true},
		{"..", "", true},
		{string(make([]rune, 121)), "", true},
	}
	for _, tc := range tests {
		got, err := cleanFolderName(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("cleanFolderName(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// The extension is half of the upload allowlist check, so a rename must
// never change it: an allowlisted file cannot become a .html by renaming.
func TestCleanFilename_KeepsExtension(t *testing.T) {
	tests := []struct {
		in, original, want string
		wantErr            bool
	}{
		{"ecg", "scan.pdf", "ecg.pdf", false},
		{"ecg.PDF", "scan.pdf", "ecg.PDF", false},
		{"page.html", "scan.pdf", "page.html.pdf", false},
		{"x/y", "scan.pdf", "", true},
		{"  ", "scan.pdf", "", true},
	}
	for _, tc := range tests {
		got, err := cleanFilename(tc.in, tc.original)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("cleanFilename(%q, %q) = %q, %v; want %q, err=%v", tc.in, tc.original, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestPreviewContentType(t *testing.T) {
	tests := []struct{ sniffed, filename, want string }{
		{"application/pdf", "a.pdf", "application/pdf"},
		{"application/octet-stream", "song.mp3", "audio/mpeg"},
		{"video/mp4", "voice.m4a", "audio/mp4"},
		{"video/mp4", "clip.mp4", "video/mp4"},
		{"application/ogg", "note.ogg", "audio/ogg"},
		{"application/octet-stream", "scan.heic", "application/octet-stream"},
		{"", "x.bin", "application/octet-stream"},
	}
	for _, tc := range tests {
		if got := PreviewContentType(tc.sniffed, tc.filename); got != tc.want {
			t.Errorf("PreviewContentType(%q, %q) = %q, want %q", tc.sniffed, tc.filename, got, tc.want)
		}
	}
}
