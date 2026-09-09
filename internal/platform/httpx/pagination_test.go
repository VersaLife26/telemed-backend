package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// per_page was capped and page was not. offset is (page-1)*perPage in int
// arithmetic, so a large enough page wrapped to a NEGATIVE offset, which
// Postgres rejects with "OFFSET must not be negative" -- a 500 on every list
// endpoint on the platform, from nothing but a query string.
func TestPagination_PageCannotOverflowIntoANegativeOffset(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"the wrapping value", "?page=92233720368547758&per_page=100"},
		{"maxint64", "?page=9223372036854775807&per_page=100"},
		{"maxint64 with the default per_page", "?page=9223372036854775807"},
		{"just under maxint", "?page=9223372036854775806&per_page=99"},
		{"negative", "?page=-1"},
		{"zero", "?page=0"},
		{"absurd per_page", "?page=2&per_page=1000000"},
		{"negative per_page", "?per_page=-5"},
		{"garbage", "?page=abc&per_page=xyz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/list"+tc.query, http.NoBody)
			page, perPage, offset := Pagination(r)

			if offset < 0 {
				t.Fatalf("%s -> offset %d; Postgres rejects a negative OFFSET and the endpoint 500s", tc.query, offset)
			}
			if page < 1 || page > MaxPage {
				t.Fatalf("%s -> page %d, outside [1,%d]", tc.query, page, MaxPage)
			}
			if perPage < 1 || perPage > 100 {
				t.Fatalf("%s -> per_page %d, outside [1,100]", tc.query, perPage)
			}
			if want := (page - 1) * perPage; offset != want {
				t.Fatalf("%s -> offset %d, want %d", tc.query, offset, want)
			}
		})
	}
}

// The ordinary cases must be untouched.
func TestPagination_NormalRequestsAreUnchanged(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/list?page=3&per_page=25", http.NoBody)
	page, perPage, offset := Pagination(r)
	if page != 3 || perPage != 25 || offset != 50 {
		t.Fatalf("page=%d per_page=%d offset=%d, want 3/25/50", page, perPage, offset)
	}

	r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/list", http.NoBody)
	page, perPage, offset = Pagination(r)
	if page != 1 || perPage != 20 || offset != 0 {
		t.Fatalf("defaults: page=%d per_page=%d offset=%d, want 1/20/0", page, perPage, offset)
	}
}
