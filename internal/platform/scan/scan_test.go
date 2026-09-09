package scan

import (
	"context"
	"strings"
	"testing"
)

func TestPassthroughScanner_DrainsAndSkips(t *testing.T) {
	body := strings.Repeat("x", 1024)
	r := strings.NewReader(body)

	verdict, err := PassthroughScanner{}.Scan(context.Background(), r, int64(len(body)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != VerdictSkipped {
		t.Errorf("verdict = %s, want %s", verdict, VerdictSkipped)
	}
	// The reader must be fully drained, not just partially -- a caller that
	// assumes Scan() consumed the stream (e.g. before computing a checksum
	// on the same reader) must not observe leftover bytes.
	if n, _ := r.Read(make([]byte, 1)); n != 0 {
		t.Error("expected the reader to be fully drained after Scan")
	}
}
