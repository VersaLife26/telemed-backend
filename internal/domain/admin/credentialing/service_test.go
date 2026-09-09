package credentialing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeStorage records what was presigned and with what TTL.
type fakeStorage struct {
	calls []struct {
		bucket, key string
		ttl         time.Duration
	}
	fail bool
}

func (f *fakeStorage) PresignedGet(_ context.Context, bucket, key string, ttl time.Duration) (string, error) {
	f.calls = append(f.calls, struct {
		bucket, key string
		ttl         time.Duration
	}{bucket, key, ttl})
	if f.fail {
		return "", fmt.Errorf("minio unreachable")
	}
	return "https://minio.example/" + bucket + "/" + key + "?X-Amz-Expires", nil
}

func fixtureSummary() DoctorSummary {
	return DoctorSummary{
		DoctorID:             uuid.New(),
		FullName:             "Dr Anula Perera",
		SLMCNumber:           "SLMC/12345",
		SLMCCertificateKey:   "doctors/slmc.pdf",
		NICDocumentKey:       "doctors/nic.jpg",
		DegreeCertificateKey: "doctors/degree.pdf",
		PhotoKey:             "doctors/photo.jpg",
		RegisteredAt:         time.Now().UTC(),
	}
}

// TestPresign_AllFourCredentialDocumentsGetAURL is the read half of Gap 3: the
// projection now holds the keys, and the reviewer must get an openable URL for
// each of the four documents the checklist asks them to confirm.
func TestPresign_AllFourCredentialDocumentsGetAURL(t *testing.T) {
	store := &fakeStorage{}
	svc := NewService(nil, nil, store, nil, "doctor-credentials", 0)

	got := svc.presign(context.Background(), fixtureSummary())

	require.NotEmpty(t, got.SLMCCertificateURL)
	require.NotEmpty(t, got.NICDocumentURL)
	require.NotEmpty(t, got.DegreeCertificateURL)
	require.NotEmpty(t, got.PhotoURL)
	require.Len(t, store.calls, 4)
	for _, c := range store.calls {
		require.Equal(t, "doctor-credentials", c.bucket,
			"doctor-documents never existed; presigning against it fails for every reviewer")
	}
	require.False(t, got.DocumentsExpireAt.IsZero(),
		"the console needs to know when these URLs die, or it shows the reviewer a 403 from MinIO")
	require.WithinDuration(t, time.Now().UTC().Add(DefaultPresignTTL), got.DocumentsExpireAt, time.Minute)
}

// TestPresign_TTLIsBoundedBecauseTheURLIsABearerCredential. A presigned URL to
// a scanned NIC grants the document to anyone holding the link. A config typo
// must not turn that into a day-long grant.
func TestPresign_TTLIsBounded(t *testing.T) {
	tests := []struct {
		name      string
		configTTL time.Duration
		want      time.Duration
	}{
		{"unset falls back to the short default", 0, DefaultPresignTTL},
		{"negative falls back to the short default", -time.Hour, DefaultPresignTTL},
		{"a sane value is honoured", 3 * time.Minute, 3 * time.Minute},
		{"an over-long value is clamped, not honoured", 24 * time.Hour, MaxPresignTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStorage{}
			svc := NewService(nil, nil, store, nil, "doctor-credentials", tt.configTTL)
			svc.presign(context.Background(), fixtureSummary())
			require.NotEmpty(t, store.calls)
			require.Equal(t, tt.want, store.calls[0].ttl)
		})
	}
	require.LessOrEqual(t, DefaultPresignTTL, MaxPresignTTL)
}

// TestPresign_NothingUploadedYetIsNotAnExpiry: an application with no
// documents must not advertise an expiry for URLs that do not exist.
func TestPresign_NothingUploadedYetIsNotAnExpiry(t *testing.T) {
	store := &fakeStorage{}
	svc := NewService(nil, nil, store, nil, "doctor-credentials", 0)

	got := svc.presign(context.Background(), DoctorSummary{DoctorID: uuid.New(), FullName: "Dr A"})

	require.Empty(t, store.calls, "there is nothing to presign")
	require.True(t, got.DocumentsExpireAt.IsZero())
	require.True(t, got.Empty())
}

// TestPresign_StorageFailureStillShowsTheApplication: MinIO being down must
// degrade the document viewer, not hide an application from the queue -- and
// the keys stay on the row so the failure reads as "document present, URL
// missing" rather than "nothing was submitted".
func TestPresign_StorageFailureStillShowsTheApplication(t *testing.T) {
	svc := NewService(nil, nil, &fakeStorage{fail: true}, nil, "doctor-credentials", 0)

	got := svc.presign(context.Background(), fixtureSummary())

	require.Empty(t, got.SLMCCertificateURL)
	require.NotEmpty(t, got.SLMCCertificateKey)
	require.Equal(t, "Dr Anula Perera", got.FullName)
}

// TestPresign_NoStorageConfiguredDoesNotPanic guards the boot path where MinIO
// is not wired at all (dev, or a deployment that has not set MINIO_*).
func TestPresign_NoStorageConfiguredDoesNotPanic(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, "doctor-credentials", 0)
	got := svc.presign(context.Background(), fixtureSummary())
	require.Empty(t, got.NICDocumentURL)
	require.NotEmpty(t, got.NICDocumentKey)
}
