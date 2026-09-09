package database

import "testing"

func TestSchema(t *testing.T) {
	t.Parallel()
	// The prefix is not cosmetic: `user` is a reserved word, and an unprefixed
	// schema of that name has to stay quoted at every later reference.
	if got, want := Schema("user"), "svc_user"; got != want {
		t.Fatalf("Schema(user) = %q, want %q", got, want)
	}
}

func TestSearchPathFor(t *testing.T) {
	t.Parallel()
	// The domain schema must come FIRST. An unqualified CREATE lands in the
	// first schema on the path, so a reversed order builds tables in public.
	if got, want := SearchPathFor("admin"), "svc_admin, public"; got != want {
		t.Fatalf("SearchPathFor(admin) = %q, want %q", got, want)
	}
}

func TestWithSearchPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, dsn, want string
	}{
		{
			name: "url form keeps existing params",
			dsn:  "postgres://telemed:pw@localhost:5432/telemed?sslmode=disable",
			want: "postgres://telemed:pw@localhost:5432/telemed?search_path=svc_user%2C+public&sslmode=disable",
		},
		{
			name: "url form replaces an existing search_path",
			dsn:  "postgres://localhost/telemed?search_path=public",
			want: "postgres://localhost/telemed?search_path=svc_user%2C+public",
		},
		{
			// The scheduling suite builds this form against a unix socket.
			// url.Parse does not reject it -- it returns an opaque URL and the
			// parameter is silently dropped, which is why the form is detected.
			name: "keyword form is appended and quoted",
			dsn:  "user=telemed host=/tmp/pg dbname=telemed_scheduling sslmode=disable",
			want: "user=telemed host=/tmp/pg dbname=telemed_scheduling sslmode=disable search_path='svc_user, public'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := WithSearchPath(tc.dsn, "svc_user, public")
			if err != nil {
				t.Fatalf("WithSearchPath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("WithSearchPath()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
