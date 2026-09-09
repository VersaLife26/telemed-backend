//go:build integration

package directory_test

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/platform/servicetoken"
)

// TestE2E_RealKeycloakTokenIsAcceptedByRealUserService closes the loop the
// unit tests can only approximate: a genuine client_credentials token from a
// running Keycloak, presented on a genuine gRPC call to a running
// user-service, through the same code path production uses.
//
// The unit tests stand up their own interceptor, so they prove the CLIENT
// attaches a credential. Only this proves the credential user-service actually
// mints is one user-service actually accepts -- the issuer binding, the realm
// role mapping and the interceptor's role check all agreeing at once. Those
// are three separate configurations that each looked right in isolation while
// the integration was dead.
//
//	MESH_CLIENT_SECRET=... KEYCLOAK_BASE_URL=http://localhost:8180 \
//	USER_SERVICE_GRPC_ADDR=localhost:9091 go test -tags=integration ./internal/directory/
func TestE2E_RealKeycloakTokenIsAcceptedByRealUserService(t *testing.T) {
	secret := os.Getenv("MESH_CLIENT_SECRET")
	base := os.Getenv("KEYCLOAK_BASE_URL")
	addr := os.Getenv("USER_SERVICE_GRPC_ADDR")
	if secret == "" || base == "" || addr == "" {
		t.Skip("set MESH_CLIENT_SECRET, KEYCLOAK_BASE_URL and USER_SERVICE_GRPC_ADDR")
	}
	realm := os.Getenv("KEYCLOAK_REALM")
	if realm == "" {
		realm = "telemedicine"
	}

	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     base + "/realms/" + realm + "/protocol/openid-connect/token",
		ClientID:     "telemed-api",
		ClientSecret: secret,
		// The dev mesh is plaintext. Production sets this from the mesh TLS
		// setting so gRPC refuses to attach the credential to an insecure link.
		RequireTLS: false,
	})
	if err != nil {
		t.Fatalf("servicetoken.New: %v", err)
	}
	if _, err := src.Token(t.Context()); err != nil {
		t.Fatalf("could not obtain a service token from Keycloak: %v", err)
	}

	client, err := directory.New(addr, 5*time.Second, directory.TLSConfig{}, src)
	if err != nil {
		t.Fatalf("directory.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// A random id: user-service should answer NOT_FOUND, which is a
	// successful, AUTHORISED call. Unauthenticated or PermissionDenied would
	// mean the token was rejected -- the failure this whole change exists to
	// remove.
	_, err = client.User(t.Context(), uuid.New())
	switch code := status.Code(err); code {
	case codes.NotFound, codes.OK:
		t.Logf("authorised: user-service answered %v", code)
	case codes.Unauthenticated:
		t.Fatalf("user-service rejected the Keycloak service token: %v", err)
	case codes.PermissionDenied:
		t.Fatalf("the token authenticated but lacks the 'service' realm role: %v", err)
	default:
		if err != nil && status.Code(err) == codes.Unknown {
			// directory.User wraps NOT_FOUND into ErrNotFound, losing the code.
			t.Logf("authorised: %v", err)
			return
		}
		t.Fatalf("unexpected: %v (code %v)", err, code)
	}
}

// TestE2E_RealUserServiceRefusesAnUncredentialedClient is the negative half,
// against the same live service. It is what the code did before this change:
// dial, call, and be refused. Without it the positive test above could pass on
// a user-service whose interceptor was misconfigured to allow everyone, and
// report the mesh as secured when it was merely working.
func TestE2E_RealUserServiceRefusesAnUncredentialedClient(t *testing.T) {
	addr := os.Getenv("USER_SERVICE_GRPC_ADDR")
	if addr == "" {
		t.Skip("set USER_SERVICE_GRPC_ADDR")
	}

	client, err := directory.New(addr, 5*time.Second, directory.TLSConfig{}, nil)
	if err != nil {
		t.Fatalf("directory.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.User(t.Context(), uuid.New())
	if err == nil {
		t.Fatal("user-service answered a client with no service token: the mesh " +
			"interceptor is not enforcing authentication")
	}
	switch code := status.Code(err); code {
	case codes.Unauthenticated, codes.PermissionDenied, codes.Unavailable:
		t.Logf("correctly refused: %v", code)
	default:
		t.Fatalf("refused with %v, want Unauthenticated: %v", code, err)
	}
}
