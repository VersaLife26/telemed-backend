package records

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRoutes_FoldersIsNotADocumentID(t *testing.T) {
	r := NewHandler(nil, nil).Routes()
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/folders", "/folders"},
		{http.MethodPost, "/folders", "/folders"},
		{http.MethodGet, "/patients", "/patients"},
		{http.MethodGet, "/shares", "/shares"},
		{http.MethodPost, "/upload", "/upload"},
		{http.MethodGet, "/6f9619ff-8b86-d011-b42d-00c04fc964ff", "/{id:[0-9a-fA-F-]{36}}"},
		{http.MethodGet, "/6f9619ff-8b86-d011-b42d-00c04fc964ff/download", "/{id:[0-9a-fA-F-]{36}}/download"},
	}
	for _, tc := range cases {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("%s %s did not match", tc.method, tc.path)
			continue
		}
		if got := rctx.RoutePattern(); got != tc.want {
			t.Errorf("%s %s routed to %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}

	stolen := chi.NewRouteContext()
	if r.Match(stolen, http.MethodGet, "/folders") && stolen.URLParam("id") != "" {
		t.Fatalf("GET /folders captured id=%q", stolen.URLParam("id"))
	}
}
