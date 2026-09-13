package user

import (
	"net/http"
	"path/filepath"
	"strings"
)

// MaxProfilePhotoBytes is the hard cap on a self-service avatar upload.
const MaxProfilePhotoBytes = 2 << 20 // 2 MiB

// ProfilePhotoURLPath is the authenticated download path returned on the
// profile payload when a photo is present. The BFF prefixes /api/proxy.
const ProfilePhotoURLPath = "/users/me/photo"

var profilePhotoByExtension = map[string]map[string]bool{
	".jpg":  {"image/jpeg": true},
	".jpeg": {"image/jpeg": true},
	".png":  {"image/png": true},
	".webp": {"image/webp": true},
}

// IsAllowedProfilePhoto reports whether ext (lowercase, with leading dot)
// paired with a sniffed content type is a permitted avatar upload.
func IsAllowedProfilePhoto(ext, sniffedContentType string) bool {
	allowed, ok := profilePhotoByExtension[ext]
	if !ok {
		return false
	}
	return allowed[sniffedContentType]
}

// ProfilePhotoExtension normalises a filename's extension for the allow-list.
func ProfilePhotoExtension(filename string) string {
	return strings.ToLower(filepath.Ext(filename))
}

// SniffProfilePhotoContentType inspects the leading bytes the same way the
// records service does: never trust the client-supplied Content-Type header.
func SniffProfilePhotoContentType(data []byte) string {
	return http.DetectContentType(data)
}
