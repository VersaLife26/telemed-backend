package user

import "testing"

func TestIsAllowedProfilePhoto(t *testing.T) {
	tests := []struct {
		ext, sniffed string
		want         bool
	}{
		{".jpg", "image/jpeg", true},
		{".jpeg", "image/jpeg", true},
		{".png", "image/png", true},
		{".webp", "image/webp", true},
		{".png", "image/jpeg", false},
		{".gif", "image/gif", false},
		{".pdf", "application/pdf", false},
	}
	for _, tc := range tests {
		if got := IsAllowedProfilePhoto(tc.ext, tc.sniffed); got != tc.want {
			t.Errorf("IsAllowedProfilePhoto(%q, %q) = %v, want %v", tc.ext, tc.sniffed, got, tc.want)
		}
	}
}

func TestProfilePhotoExtension(t *testing.T) {
	if got := ProfilePhotoExtension("Avatar.PNG"); got != ".png" {
		t.Errorf("got %q, want .png", got)
	}
}
