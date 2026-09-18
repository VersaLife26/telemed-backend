package appointments

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRoutesMountsAudit(t *testing.T) {
	t.Parallel()

	found := false
	err := chi.Walk(NewHandler(nil).Routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && strings.Contains(route, "audit") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if !found {
		t.Fatal("GET /{id}/audit is not mounted; the console 404s as NOT_FOUND")
	}
}
