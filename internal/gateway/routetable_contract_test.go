package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	platmw "telemed/internal/platform/middleware"
)

// testUserID and allTestRoles drive TestRouter_EveryRouteResolvesThroughChi:
// one principal that satisfies every role gate in the table, so the test is
// about routing, not authorization (which router_test.go covers separately).
var (
	testUserID   = uuid.MustParse("7f3a1c22-9a1e-4b8b-9f7a-2b7b0a6a1234")
	allTestRoles = []platmw.Role{
		platmw.RolePatient, platmw.RoleDoctor, platmw.RoleAdmin,
		platmw.RoleSuperAdmin, platmw.RoleOps, platmw.RoleFinance, platmw.RoleSupport,
	}
)

// The route table is data, and data drifts silently. These tests pin it
// against the two things it must agree with: what the mobile apps call, and
// what the owning service actually registers.
//
// Every entry below was verified by reading the handler registration in the
// owning repository -- not inferred from a path that looked plausible. A route
// in this table that no service registers is worse than a missing one: a
// missing route 404s at the gateway with a clear message, while a wrong one
// proxies a request into an upstream that 404s it, and the client cannot tell
// the difference between "not implemented" and "you called it wrong".

// backendRoute is one verified upstream registration.
type backendRoute struct {
	method   string
	pattern  string
	upstream string
	// source is where the registration lives, so the next person to doubt this
	// table can check it in one grep rather than re-deriving the whole thing.
	source string
}

// mobileClientSurface is every route the patient and doctor Next.js apps call
// that a backend actually implements. Anything the apps call with no backend
// is deliberately absent -- see the gateway's report; inventing a route for it
// would turn "not built yet" into "built and broken".
var mobileClientSurface = []backendRoute{
	// user-service -- internal/user/handler.go
	{"POST", "/api/v1/auth/otp/send", "user-service", "user/handler.go:45"},
	{"POST", "/api/v1/auth/otp/verify", "user-service", "user/handler.go:49"},
	{"POST", "/api/v1/auth/register/email", "user-service", "user/handler.go:53"},
	{"POST", "/api/v1/auth/login/email", "user-service", "user/handler.go:57"},
	{"POST", "/api/v1/auth/oauth/google", "user-service", "user/handler.go:61"},
	{"POST", "/api/v1/auth/refresh", "user-service", "user/handler.go:63"},
	{"POST", "/api/v1/auth/logout", "user-service", "user/handler.go:64"},
	{"POST", "/api/v1/auth/logout-all", "user-service", "user/handler.go:65"},
	{"GET", "/api/v1/users/me", "user-service", "user/handler.go:72"},
	{"PUT", "/api/v1/users/me", "user-service", "user/handler.go:73"},
	{"PUT", "/api/v1/users/me/password", "user-service", "user/handler.go:74"},
	{"DELETE", "/api/v1/users/me", "user-service", "user/handler.go:75"},
	{"GET", "/api/v1/users/me/family", "user-service", "user/handler.go:65"},
	{"POST", "/api/v1/users/me/family", "user-service", "user/handler.go:66"},
	{"PUT", "/api/v1/users/me/family/{familyMemberID}", "user-service", "user/handler.go:67"},
	{"DELETE", "/api/v1/users/me/family/{familyMemberID}", "user-service", "user/handler.go:68"},

	// doctor-service -- internal/doctor/handler.go
	{"GET", "/api/v1/doctors", "doctor-service", "doctor/handler.go:32"},
	{"POST", "/api/v1/doctors/apply", "doctor-service", "doctor/handler.go:apply"},
	{"POST", "/api/v1/doctors/applications/{applicationID}/documents", "doctor-service", "doctor/handler.go:applyDocument"},
	{"GET", "/api/v1/doctors/applications/eligibility", "doctor-service", "doctor/handler.go:eligibility"},
	{"POST", "/api/v1/doctors/register", "doctor-service", "doctor/handler.go:33"},
	{"GET", "/api/v1/doctors/me", "doctor-service", "doctor/handler.go:37"},
	{"PUT", "/api/v1/doctors/me", "doctor-service", "doctor/handler.go:38"},
	{"GET", "/api/v1/doctors/me/availability", "doctor-service", "doctor/handler.go:39"},
	{"PUT", "/api/v1/doctors/me/availability", "doctor-service", "doctor/handler.go:40"},
	{"POST", "/api/v1/doctors/me/documents", "doctor-service", "doctor/handler.go:41"},
	{"PUT", "/api/v1/doctors/me/photo", "doctor-service", "doctor/handler.go:photo"},
	{"GET", "/api/v1/doctors/me/photo", "doctor-service", "doctor/handler.go:photo"},
	{"DELETE", "/api/v1/doctors/me/photo", "doctor-service", "doctor/handler.go:photo"},
	{"PUT", "/api/v1/doctors/me/signature", "doctor-service", "doctor/handler.go:signature"},
	{"GET", "/api/v1/doctors/me/signature", "doctor-service", "doctor/handler.go:signature"},
	{"PUT", "/api/v1/doctors/me/seal", "doctor-service", "doctor/handler.go:seal"},
	{"GET", "/api/v1/doctors/me/seal", "doctor-service", "doctor/handler.go:seal"},
	{"GET", "/api/v1/doctors/{doctorID}/photo", "doctor-service", "doctor/handler.go:photo"},
	{"GET", "/api/v1/doctors/{doctorID}", "doctor-service", "doctor/handler.go:44"},
	{"GET", "/api/v1/doctors/{doctorID}/reviews", "doctor-service", "doctor/handler.go:45"},
	{"POST", "/api/v1/doctors/{doctorID}/reviews", "doctor-service", "doctor/handler.go:46"},

	// scheduling-service -- internal/scheduling/handler.go
	{"GET", "/api/v1/doctors/{doctorID}/slots", "scheduling-service", "scheduling/handler.go:52"},
	{"POST", "/api/v1/appointments", "scheduling-service", "scheduling/handler.go:58"},
	{"GET", "/api/v1/appointments", "scheduling-service", "scheduling/handler.go:59"},
	{"GET", "/api/v1/appointments/last-visit-details", "scheduling-service", "scheduling/handler.go:last-visit-details"},
	{"GET", "/api/v1/appointments/{appointmentID}", "scheduling-service", "scheduling/handler.go:60"},
	{"PUT", "/api/v1/appointments/{appointmentID}/cancel", "scheduling-service", "scheduling/handler.go:61"},
	{"POST", "/api/v1/appointments/{appointmentID}/complete", "scheduling-service", "scheduling/handler.go:62"},
	{"POST", "/api/v1/appointments/{appointmentID}/no-show", "scheduling-service", "scheduling/handler.go:63"},
	{"POST", "/api/v1/appointments/{appointmentID}/reschedule-requests", "scheduling-service", "scheduling/handler.go:reschedule"},
	{"GET", "/api/v1/appointments/{appointmentID}/reschedule-requests", "scheduling-service", "scheduling/handler.go:reschedule"},
	{"POST", "/api/v1/reschedule-requests/{requestID}/accept", "scheduling-service", "scheduling/handler.go:reschedule"},
	{"POST", "/api/v1/reschedule-requests/{requestID}/decline", "scheduling-service", "scheduling/handler.go:reschedule"},
	{"POST", "/api/v1/waitlist", "scheduling-service", "scheduling/handler.go:66"},

	// consultation-service -- internal/consultation/handler.go
	{"POST", "/api/v1/consultations/{consultationID}/join", "consultation-service", "consultation/handler.go:41"},
	{"POST", "/api/v1/consultations/{consultationID}/admit", "consultation-service", "consultation/handler.go:42"},
	{"POST", "/api/v1/consultations/{consultationID}/end", "consultation-service", "consultation/handler.go:43"},
	{"POST", "/api/v1/consultations/{consultationID}/consent", "consultation-service", "consultation/handler.go:44"},
	{"GET", "/api/v1/consultations/{consultationID}", "consultation-service", "consultation/handler.go:45"},
	{"GET", "/api/v1/consultations/{consultationID}/waiting-room", "consultation-service", "consultation/handler.go:46"},
	{"POST", "/api/v1/consultations/{consultationID}/quality", "consultation-service", "consultation/handler.go:47"},
	{"POST", "/api/v1/consultations/{consultationID}/messages", "consultation-service", "consultation/handler.go:messages"},
	{"GET", "/api/v1/consultations/{consultationID}/messages", "consultation-service", "consultation/handler.go:messages"},
	{"POST", "/api/v1/consultations/ready-for-next", "consultation-service", "consultation/handler.go:early-join"},
	{"POST", "/api/v1/consultations/{consultationID}/ready-for-next", "consultation-service", "consultation/handler.go:early-join"},
	{"GET", "/api/v1/consultations/{consultationID}/early-join", "consultation-service", "consultation/handler.go:early-join"},
	{"POST", "/api/v1/consultations/{consultationID}/early-join/accept", "consultation-service", "consultation/handler.go:early-join"},
	{"POST", "/api/v1/consultations/{consultationID}/early-join/decline", "consultation-service", "consultation/handler.go:early-join"},
	{"POST", "/api/v1/webhooks/livekit", "consultation-service", "consultation/handler.go:60"},

	// payment-service -- internal/payment/handler.go, handler_client.go, webhook.go
	{"POST", "/api/v1/payments/intent", "payment-service", "payment/handler.go:41"},
	{"GET", "/api/v1/payments", "payment-service", "payment/handler.go:42"},
	{"GET", "/api/v1/payments/{paymentID}", "payment-service", "payment/handler.go:44"},
	{"POST", "/api/v1/payments/{paymentID}/confirm-pin", "payment-service", "payment/handler.go:46"},
	{"POST", "/api/v1/payments/{paymentID}/refund", "payment-service", "payment/handler.go:45"},
	{"GET", "/api/v1/payments/{paymentID}/invoice", "payment-service", "payment/handler.go:47"},
	{"GET", "/api/v1/payments/{paymentID}/ledger", "payment-service", "payment/handler.go:48"},
	{"GET", "/api/v1/payments/order/{appointmentID}", "payment-service", "payment/handler.go:52"},
	{"POST", "/api/v1/payments/promo/apply", "payment-service", "payment/handler_client.go:23"},
	{"POST", "/api/v1/payments/promo/remove", "payment-service", "payment/handler_client.go:24"},
	{"GET", "/api/v1/payments/promo/codes", "payment-service", "payment/handler_client.go:31"},
	{"POST", "/api/v1/payments/promo/codes", "payment-service", "payment/handler_client.go:30"},
	{"DELETE", "/api/v1/payments/promo/codes/{code}", "payment-service", "payment/handler_client.go:32"},
	{"POST", "/api/v1/payments/dialog/request-pin", "payment-service", "payment/handler_client.go:47"},
	{"POST", "/api/v1/payments/dialog/confirm", "payment-service", "payment/handler_client.go:48"},
	{"GET", "/api/v1/payments/methods", "payment-service", "payment/handler_client.go:57"},
	{"POST", "/api/v1/payments/methods", "payment-service", "payment/handler_client.go:58"},
	{"POST", "/api/v1/payments/methods/setup-intent", "payment-service", "payment/handler_client.go:59"},
	{"DELETE", "/api/v1/payments/methods/{methodID}", "payment-service", "payment/handler_client.go:60"},
	{"PUT", "/api/v1/payments/methods/{methodID}/default", "payment-service", "payment/handler_client.go:61"},
	{"POST", "/api/v1/payouts/run", "payment-service", "payment/handler.go:57"},
	{"GET", "/api/v1/payouts", "payment-service", "payment/handler.go:58"},
	{"POST", "/api/v1/webhooks/stripe", "payment-service", "payment/webhook.go:66"},
	{"POST", "/api/v1/webhooks/payhere", "payment-service", "payment/webhook.go:67"},
	{"POST", "/api/v1/webhooks/dialog", "payment-service", "payment/webhook.go:68"},

	// notification-service -- internal/notification/handler.go
	{"GET", "/api/v1/notifications", "notification-service", "notification/handler.go:43"},
	{"PUT", "/api/v1/notifications/read-all", "notification-service", "notification/handler.go:48"},
	{"PUT", "/api/v1/notifications/{notificationID}/read", "notification-service", "notification/handler.go:49"},
	{"GET", "/api/v1/notifications/preferences", "notification-service", "notification/handler.go:51"},
	{"PUT", "/api/v1/notifications/preferences", "notification-service", "notification/handler.go:52"},
	// Registered since day one and never routed, which is why the patient app
	// invented PUT /users/me/push-token against user-service: push
	// registration was unreachable through the only ingress it has.
	{"POST", "/api/v1/notifications/devices", "notification-service", "notification/handler.go:54"},
	{"DELETE", "/api/v1/notifications/devices/{deviceID}", "notification-service", "notification/handler.go:55"},

	// record-service -- internal/records/handler.go, internal/prescriptions/handler.go
	{"POST", "/api/v1/records/upload", "record-service", "records/handler.go:34"},
	{"GET", "/api/v1/records", "record-service", "records/handler.go:35"},
	{"GET", "/api/v1/records/{recordID}", "record-service", "records/handler.go:37"},
	{"GET", "/api/v1/records/{recordID}/download", "record-service", "records/handler.go:38"},
	{"POST", "/api/v1/prescriptions", "record-service", "prescriptions/handler.go:26"},
	{"GET", "/api/v1/prescriptions", "record-service", "prescriptions/handler.go:27"},
	{"GET", "/api/v1/prescriptions/{prescriptionID}", "record-service", "prescriptions/handler.go:28"},
	{"GET", "/api/v1/prescriptions/{prescriptionID}/pdf", "record-service", "prescriptions/handler.go:29"},
	{"DELETE", "/api/v1/records/{recordID}", "record-service", "records/handler.go:39"},
	{"GET", "/api/v1/drugs", "record-service", "prescriptions/handler.go:36"},
	{"GET", "/api/v1/verify/prescriptions/{prescriptionID}", "record-service", "prescriptions/handler.go:46"},
}

// stillUnbuilt is the other half of the honesty: endpoints the mobile apps
// call that STILL have no backend, asserted absent so nobody adds a route to
// nothing. A missing route 404s at the gateway with a clear message; a route
// to an unimplemented upstream 404s from the upstream, and the client cannot
// tell "not built" from "you called it wrong".
//
// Each of these is a decision recorded in the integration report, not an
// oversight:
//
//   - insurance and support threads are substantial products in their own
//     right and were left out entirely rather than stubbed.
//   - waiting-status, leave, token/refresh and users/me/push-token are client
//     mistakes with working backends under different names; the fix is in the
//     app, not a gateway alias, because in three of the four cases the client
//     is also passing the wrong identifier and an alias would not make the
//     call work.
//   - clinical notes remain unowned by any service.
var stillUnbuilt = []string{
	"GET /api/v1/consultations/{consultationID}/waiting-status", // use /waiting-room
	"POST /api/v1/consultations/{consultationID}/leave",         // use /end, or just disconnect
	"POST /api/v1/auth/token/refresh",                           // use /auth/refresh
	"PUT /api/v1/users/me/push-token",                           // use POST /notifications/devices
	"POST /api/v1/payments/insurance",                           // out of scope for this pass
	"GET /api/v1/support/threads/{threadID}/messages",           // out of scope for this pass
	"POST /api/v1/support/threads/{threadID}/messages",          // out of scope for this pass
	"POST /api/v1/clinical-notes",                               // no service owns it
}

// TestRouteTable_DoesNotRouteWhatNobodyImplements asserts that every entry in
// stillUnbuilt stays absent from the table, in both directions: no pattern may
// match one of those calls, and no methodless catchall may swallow it either.
func TestRouteTable_DoesNotRouteWhatNobodyImplements(t *testing.T) {
	rules := loadTestRoutes(t)
	for _, key := range stillUnbuilt {
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed entry %q", key)
		}
		for _, r := range rules {
			if r.Pattern != pattern {
				continue
			}
			if len(r.Methods) == 0 {
				t.Errorf("route %q (%s) catches %s, which no service implements", r.Name, r.Pattern, key)
				continue
			}
			for _, m := range r.Methods {
				if m == method {
					t.Errorf("route %q routes %s, which no service implements: a route to nothing "+
						"turns \"not built yet\" into \"built and broken\"", r.Name, key)
				}
			}
		}
	}
}

// routeIndex keys the table by "METHOD /pattern". A methodless rule (the admin
// catchall) is keyed with "*".
func routeIndex(rules []RouteRule) map[string]RouteRule {
	idx := make(map[string]RouteRule, len(rules))
	for i := range rules {
		r := &rules[i]
		if len(r.Methods) == 0 {
			idx["* "+r.Pattern] = *r
			continue
		}
		for _, m := range r.Methods {
			idx[m+" "+r.Pattern] = *r
		}
	}
	return idx
}

// TestRouteTable_CoversEveryEndpointTheMobileAppsCall is the regression test
// for the route-table gaps: doctor profile edit, doctor documents, appointment
// list/detail/complete/no-show, drug search, records list, payouts, and
// notification mark-read were all called by an app and absent here, so every
// one of those screens 404'd at the gateway against a backend that implements
// them perfectly well.
func TestRouteTable_CoversEveryEndpointTheMobileAppsCall(t *testing.T) {
	idx := routeIndex(loadTestRoutes(t))

	var missing []string
	for i := range mobileClientSurface {
		want := &mobileClientSurface[i]
		key := want.method + " " + want.pattern
		got, ok := idx[key]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s  (registered at %s)", key, want.source))
			continue
		}
		if got.Upstream != want.upstream {
			t.Errorf("%s routes to %q, want %q -- the request would reach a service that "+
				"does not implement it", key, got.Upstream, want.upstream)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the mobile apps call %d endpoint(s) the gateway does not route:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestRouteTable_CorrectedEntries pins the four entries that pointed at paths
// or methods no service registers. Each of these proxied a real request into
// an upstream that rejected it, which is the hardest failure mode to diagnose
// from a client: the gateway says the route exists, the backend says it does
// not.
func TestRouteTable_CorrectedEntries(t *testing.T) {
	rules := loadTestRoutes(t)
	byName := map[string]RouteRule{}
	for _, r := range rules {
		byName[r.Name] = r
	}

	t.Run("notification preferences are saved with PUT, not POST", func(t *testing.T) {
		r, ok := byName["notification-preferences-put"]
		if !ok {
			t.Fatal("notification-preferences-put is missing")
		}
		if len(r.Methods) != 1 || r.Methods[0] != http.MethodPut {
			t.Errorf("methods = %v, want [PUT]: notification-service registers PUT and "+
				"has no POST handler, so the old entry 405'd every preference save", r.Methods)
		}
		for _, other := range rules {
			if other.Pattern == "/api/v1/notifications/preferences" {
				for _, m := range other.Methods {
					if m == http.MethodPost {
						t.Errorf("route %q still declares POST on the preferences path", other.Name)
					}
				}
			}
		}
	})

	t.Run("prescription verification lives under /verify/prescriptions", func(t *testing.T) {
		r, ok := byName["prescription-verify-get"]
		if !ok {
			t.Fatal("prescription-verify-get is missing")
		}
		const want = "/api/v1/verify/prescriptions/{prescriptionID}"
		if r.Pattern != want {
			t.Errorf("pattern = %q, want %q -- record-service mounts the public verify "+
				"surface at /verify/prescriptions, not /prescriptions/verify, so a "+
				"pharmacist's QR scan 404'd", r.Pattern, want)
		}
	})

	t.Run("family profiles live under /users/me/family", func(t *testing.T) {
		for _, r := range rules {
			if strings.HasPrefix(r.Pattern, "/api/v1/users/family") {
				t.Errorf("route %q uses %q; user-service registers the family subtree under "+
					"/api/v1/users/me/family", r.Name, r.Pattern)
			}
		}
		if _, ok := byName["user-family-post"]; !ok {
			t.Fatal("user-family-post is missing")
		}
		if got := byName["user-family-post"].Pattern; got != "/api/v1/users/me/family" {
			t.Errorf("pattern = %q, want /api/v1/users/me/family", got)
		}
	})

	t.Run("there is no /doctors/me/appointments", func(t *testing.T) {
		// doctor-service registers /me/{,availability,documents} and nothing
		// else. The doctor app's dashboard reads GET /api/v1/appointments from
		// scheduling-service, which scopes by the principal's role.
		for _, r := range rules {
			if strings.Contains(r.Pattern, "/doctors/me/appointments") {
				t.Errorf("route %q points at /doctors/me/appointments, which doctor-service "+
					"does not register", r.Name)
			}
		}
	})
}

// TestRouteTable_ConsultationSurfaceIsReachable is the gateway half of the
// JoinResult gap. Even with the consultation id now returned by join, a client
// could not act on it: only join and admit were routed, so consent -- which
// gates recording -- was unreachable through the only ingress the mobile apps
// have.
func TestRouteTable_ConsultationSurfaceIsReachable(t *testing.T) {
	idx := routeIndex(loadTestRoutes(t))
	for _, key := range []string{
		"POST /api/v1/consultations/{consultationID}/join",
		"POST /api/v1/consultations/{consultationID}/admit",
		"POST /api/v1/consultations/{consultationID}/end",
		"POST /api/v1/consultations/{consultationID}/consent",
		"GET /api/v1/consultations/{consultationID}",
		"GET /api/v1/consultations/{consultationID}/waiting-room",
		"POST /api/v1/consultations/{consultationID}/quality",
		"POST /api/v1/consultations/{consultationID}/messages",
		"GET /api/v1/consultations/{consultationID}/messages",
		"POST /api/v1/consultations/ready-for-next",
		"POST /api/v1/consultations/{consultationID}/ready-for-next",
		"GET /api/v1/consultations/{consultationID}/early-join",
		"POST /api/v1/consultations/{consultationID}/early-join/accept",
		"POST /api/v1/consultations/{consultationID}/early-join/decline",
	} {
		r, ok := idx[key]
		if !ok {
			t.Errorf("%s is not routed", key)
			continue
		}
		if r.Upstream != "consultation-service" {
			t.Errorf("%s routes to %q", key, r.Upstream)
		}
		if r.Auth != AuthAuthenticated {
			t.Errorf("%s has auth mode %q, want authenticated: a consultation is between "+
				"two identified parties", key, r.Auth)
		}
	}
}

// TestRouteTable_DoesNotRouteTheSignallingSocket asserts an ABSENCE, which is
// unusual enough to explain.
//
// /ws/consultation is served raw, from a bare mux ahead of the router,
// because a websocket upgrade hijacks the connection. Adding it to this table
// would send the upgrade through the gateway's in-process proxy, whose
// statusRecorder implements http.Flusher but NOT http.Hijacker -- so
// Upgrade() fails with "response does not implement http.Hijacker" and every
// consultation stops connecting.
//
// Even through the reverse proxy it would break differently: the table
// assigns each route a timeout class, and the shortest is five seconds.
func TestRouteTable_DoesNotRouteTheSignallingSocket(t *testing.T) {
	for _, r := range loadTestRoutes(t) {
		if strings.Contains(r.Pattern, "/ws/") {
			t.Errorf("route %q (%s) puts a websocket path in the gateway route table; "+
				"it must be served raw -- see modular.Module.Raw", r.Pattern, r.Name)
		}
	}
}

// TestRouteTable_DoctorPrefixOwnership guards the one genuinely ambiguous
// prefix on the platform. /api/v1/doctors is doctor-service's, except for
// /slots, which is scheduling-service's -- and chi resolves the more specific
// pattern first. Getting this backwards routes every profile read into the
// scheduler.
func TestRouteTable_DoctorPrefixOwnership(t *testing.T) {
	for _, r := range loadTestRoutes(t) {
		if !strings.HasPrefix(r.Pattern, "/api/v1/doctors") {
			continue
		}
		want := "doctor-service"
		if strings.HasSuffix(r.Pattern, "/slots") {
			want = "scheduling-service"
		}
		if r.Upstream != want {
			t.Errorf("route %q (%s) routes to %q, want %q", r.Name, r.Pattern, r.Upstream, want)
		}
	}
}

// TestRouter_EveryRouteResolvesThroughChi is the strongest assertion available
// without the real backends: mount the real table on the real router and drive
// a real request at every pattern. It catches a chi pattern conflict (two
// different parameter names at one position, which panics at Mount), a pattern
// that silently falls into the /admin catchall, and any entry whose method set
// does not match what it claims.
func TestRouter_EveryRouteResolvesThroughChi(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	cfg := testConfig()
	cfg.AdminIPAllowlist = []string{"192.0.2.0/24"}
	r, _, _ := buildTestRouter(t, ti, cfg, upSrv.URL, nil)

	for _, rule := range loadTestRoutes(t) {
		if strings.HasSuffix(rule.Pattern, "*") {
			continue // the catchall has no single concrete path to probe
		}
		methods := rule.Methods
		if len(methods) == 0 {
			methods = []string{http.MethodGet}
		}
		for _, m := range methods {
			t.Run(rule.Name+"/"+m, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), m, concretePath(rule.Pattern), http.NoBody)
				req.RemoteAddr = "192.0.2.10:5555"
				if rule.Auth != AuthPublic {
					req.Header.Set("Authorization", "Bearer "+ti.sign(t, testUserID, allTestRoles))
				}
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)

				if rec.Code == http.StatusNotFound {
					t.Fatalf("%s %s returned the gateway's own 404: the pattern is not "+
						"mounted as declared (body: %s)", m, rule.Pattern, rec.Body.String())
				}
				if rec.Code == http.StatusMethodNotAllowed {
					t.Fatalf("%s %s returned 405: the pattern is mounted for other methods "+
						"but not this one", m, rule.Pattern)
				}
			})
		}
	}
}

// concretePath substitutes a real uuid for every {param} so the pattern can be
// driven as an actual request.
func concretePath(pattern string) string {
	out := pattern
	for {
		open := strings.Index(out, "{")
		if open < 0 {
			return out
		}
		closeIdx := strings.Index(out[open:], "}")
		if closeIdx < 0 {
			return out
		}
		out = out[:open] + "8c5b1d90-2b41-4f0e-9a0e-5f2f0b6c7d81" + out[open+closeIdx+1:]
	}
}
