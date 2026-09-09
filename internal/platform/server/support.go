package server

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/go-chi/cors"
)

func runtimeVersion() string { return runtime.Version() }

func stack() []byte { return debug.Stack() }

// corsMiddleware allows only the configured origins. A wildcard is permitted in
// dev because Next.js may serve from an ephemeral port, but it is
// refused with credentials, which is what makes wildcard CORS dangerous.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	if len(origins) == 0 {
		origins = []string{"http://localhost:3000", "http://localhost:8080"}
	}
	allowCredentials := !slices.ContainsFunc(origins, func(o string) bool {
		return strings.TrimSpace(o) == "*"
	})

	return cors.Handler(cors.Options{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{
			"Accept", "Authorization", "Content-Type", "X-Request-ID",
			"X-Idempotency-Key", "Accept-Language",
		},
		ExposedHeaders: []string{
			"X-Request-ID", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
		},
		AllowCredentials: allowCredentials,
		MaxAge:           300,
	})
}
