package doctor

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxProfilePhotoBytes is the hard cap on a self-service directory portrait.
const MaxProfilePhotoBytes = 2 << 20 // 2 MiB

var profilePhotoByExtension = map[string]map[string]bool{
	".jpg":  {"image/jpeg": true},
	".jpeg": {"image/jpeg": true},
	".png":  {"image/png": true},
	".webp": {"image/webp": true},
}

// IsAllowedProfilePhoto reports whether ext (lowercase, with leading dot)
// paired with a sniffed content type is a permitted portrait upload.
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

// SniffProfilePhotoContentType inspects the leading bytes; the client-supplied
// Content-Type header is never trusted.
func SniffProfilePhotoContentType(data []byte) string {
	return http.DetectContentType(data)
}

// ProfilePhotoPath is the public download path returned on photo_url so the
// directory can render the portrait without embedding bytes in search results.
func ProfilePhotoPath(doctorID uuid.UUID, updatedAt time.Time) string {
	return fmt.Sprintf("/doctors/%s/photo?v=%d", doctorID.String(), updatedAt.Unix())
}
