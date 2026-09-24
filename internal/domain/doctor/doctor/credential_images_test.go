package doctor

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"regexp"
	"testing"

	"github.com/google/uuid"
)

func TestCredentialImageExt_DecidesByContentNotName(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var pngBuf, jpgBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpgBuf, img, nil); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"png":             {pngBuf.Bytes(), ".png"},
		"jpeg":            {jpgBuf.Bytes(), ".jpg"},
		"pdf":             {[]byte("%PDF-1.4\n"), ""},
		"png header only": {pngBuf.Bytes()[:8], ""},
		"empty":           {nil, ""},
	}
	for name, tc := range cases {
		if got := credentialImageExt(tc.data); got != tc.want {
			t.Errorf("%s: ext = %q, want %q", name, got, tc.want)
		}
	}
}

func TestSignatureSealKey_IsServerDerived(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key := signatureSealKey(id, DocumentSeal, ".png")
	re := regexp.MustCompile(`^doctors/11111111-1111-1111-1111-111111111111/seal-[0-9a-f-]{36}\.png$`)
	if !re.MatchString(key) {
		t.Fatalf("key %q does not match doctors/<id>/<type>-<uuid>.<ext>", key)
	}
	if credentialImageContentType(key) != "image/png" {
		t.Fatalf("content type for %q = %q", key, credentialImageContentType(key))
	}
}
