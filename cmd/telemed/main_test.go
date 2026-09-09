package main

import (
	"net/url"
	"strings"
	"testing"
)

// dsnResolver is what keeps the per-domain least-privilege model alive in a
// single process. If it ever returns the same role for two domains, security
// review F14 is gone and nothing else in the system would notice.
func TestDSNResolver_GivesEachDomainItsOwnRoleAndSchema(t *testing.T) {
	t.Parallel()
	resolve := dsnResolver("postgres://telemed:s3cret@db:5432/telemed?sslmode=require")

	seenRole := map[string]string{}
	for _, domain := range []string{"user", "doctor", "scheduling", "consultation",
		"payment", "notification", "record", "admin"} {
		got, err := resolve(domain)
		if err != nil {
			t.Fatalf("resolve(%s): %v", domain, err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("resolve(%s) produced an unparseable DSN %q: %v", domain, got, err)
		}

		wantRole := "telemed_" + domain + "_app"
		if u.User.Username() != wantRole {
			t.Errorf("%s connects as %q, want %q", domain, u.User.Username(), wantRole)
		}
		if pw, _ := u.User.Password(); pw != "s3cret" {
			t.Errorf("%s lost the password from the base DSN", domain)
		}
		if got, want := u.Query().Get("search_path"), "svc_"+domain+", public"; got != want {
			t.Errorf("%s search_path = %q, want %q", domain, got, want)
		}
		// Everything else about the base DSN must survive: dropping sslmode
		// silently downgrades a production connection to plaintext.
		if u.Query().Get("sslmode") != "require" {
			t.Errorf("%s lost sslmode from the base DSN: %q", domain, got)
		}
		if u.Host != "db:5432" || u.Path != "/telemed" {
			t.Errorf("%s points at %s%s, want db:5432/telemed", domain, u.Host, u.Path)
		}

		if other, dup := seenRole[u.User.Username()]; dup {
			t.Fatalf("%s and %s share the role %q; the domains are no longer isolated",
				domain, other, u.User.Username())
		}
		seenRole[u.User.Username()] = domain
	}
}

func TestDSNResolver_PerDomainOverrideWins(t *testing.T) {
	// No t.Parallel: t.Setenv and parallel tests are mutually exclusive.
	// A deployment where the roles do not share one password sets these.
	t.Setenv("TELEMED_DB_URL_PAYMENT", "postgres://custom@elsewhere/telemed")
	resolve := dsnResolver("postgres://telemed:pw@db:5432/telemed")

	got, err := resolve("payment")
	if err != nil {
		t.Fatalf("resolve(payment): %v", err)
	}
	if got != "postgres://custom@elsewhere/telemed" {
		t.Fatalf("override ignored: %q", got)
	}
	// ...and only for the domain it names.
	other, err := resolve("record")
	if err != nil {
		t.Fatalf("resolve(record): %v", err)
	}
	if !strings.Contains(other, "telemed_record_app") {
		t.Fatalf("override leaked into another domain: %q", other)
	}
}

func TestDSNResolver_RefusesToGuessWithNoBaseDSN(t *testing.T) {
	t.Parallel()
	if _, err := dsnResolver("")("user"); err == nil {
		t.Fatal("an unset DATABASE_URL must be an error, not a connection to nowhere")
	}
}

func TestSelectDomains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		raw       string
		wantN     int
		wantEdge  bool
		wantError bool
	}{
		{name: "empty means everything", raw: "", wantN: len(builders), wantEdge: true},
		{name: "all means everything", raw: "all", wantN: len(builders), wantEdge: true},
		{name: "ALL is not case sensitive", raw: "ALL", wantN: len(builders), wantEdge: true},
		// The rollback shape: this binary as the old user-service.
		{name: "one domain, no edge", raw: "user", wantN: 1, wantEdge: false},
		{name: "edge alone is the old api-gateway", raw: "edge", wantN: 0, wantEdge: true},
		{name: "a subset with the edge", raw: "user, doctor ,edge", wantN: 2, wantEdge: true},
		// A typo must stop the process, not silently run a smaller platform.
		{name: "an unknown name is fatal", raw: "user,usr", wantError: true},
		{name: "selecting nothing is fatal", raw: ",", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			domains, edge, err := selectDomains(tc.raw)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected an error for %q, got %v", tc.raw, domains)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectDomains(%q): %v", tc.raw, err)
			}
			if len(domains) != tc.wantN {
				t.Errorf("got %d domains %v, want %d", len(domains), domains, tc.wantN)
			}
			if edge != tc.wantEdge {
				t.Errorf("edge = %v, want %v", edge, tc.wantEdge)
			}
		})
	}
}

// The user domain must be built before anything that looks up the in-process
// user directory, or admin and notification would find an empty registry and
// dial a port this process is not listening on.
func TestBuilderOrder_UserComesFirst(t *testing.T) {
	t.Parallel()
	if len(builders) == 0 || builders[0].name != "user" {
		t.Fatalf("user must be built first; order is %v", builderNames())
	}
}

func builderNames() []string {
	out := make([]string, 0, len(builders))
	for _, b := range builders {
		out = append(out, b.name)
	}
	return out
}
