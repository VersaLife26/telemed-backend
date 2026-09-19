package doctor

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIsAllowedProfilePhoto(t *testing.T) {
	if !IsAllowedProfilePhoto(".jpg", "image/jpeg") {
		t.Fatal("jpeg should be allowed")
	}
	if IsAllowedProfilePhoto(".pdf", "application/pdf") {
		t.Fatal("pdf must not be a portrait")
	}
	if IsAllowedProfilePhoto(".png", "image/jpeg") {
		t.Fatal("extension and sniffed type must agree")
	}
}

func TestProfilePhotoPathCacheBusts(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := ProfilePhotoPath(id, time.Unix(1_700_000_000, 0).UTC())
	want := "/doctors/11111111-1111-1111-1111-111111111111/photo?v=1700000000"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
