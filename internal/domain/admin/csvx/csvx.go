// Package csvx holds the one thing every CSV export on the admin surface must
// do before a cell reaches a spreadsheet.
package csvx

import "strings"

// dangerous are the leading characters a spreadsheet treats as the start of a
// formula rather than as text.
//
//   - '=' and '+' start a formula in Excel, LibreOffice and Google Sheets.
//   - '-' starts one too (it is unary minus, not a hyphen, to a parser).
//   - '@' is Lotus-inherited and still live in Excel; it is also the DDE
//     prefix, which is the variant that reaches the shell.
//   - TAB and CR are stripped by Excel before evaluation, so "\t=cmd|..."
//     becomes "=cmd|..." after the trim. Leaving them out of the set is how
//     most implementations of this mitigation get bypassed.
const dangerous = "=+-@\t\r"

// Cell renders one value safe to open in a spreadsheet.
//
// The threat is not theoretical for this platform. The audit export is the
// file handed to a regulator, and it carries columns holding text somebody
// else authored: a dispute description, a doctor rejection reason, a config
// value, a User-Agent. A cell beginning with one of those characters executes
// when the file is opened, so an attacker who can get a string into any
// audited field gets code execution on the auditor's machine, out of a file
// the platform vouched for.
//
// The mitigation is a leading apostrophe, which every major spreadsheet reads
// as "the rest of this cell is literal text" and does not display.
//
// On the hash chain: this deliberately does NOT touch prev_hash or row_hash,
// which are hex and cannot begin with a dangerous character anyway. It is
// worth being explicit that the CSV was never a chain-verifiable artifact in
// the first place -- audit_canonical_json formats created_at to fixed
// microseconds while the CSV emits RFC3339Nano, which drops trailing zeros, so
// a verifier recomputing SHA-256 from these bytes would already fail on any
// row whose timestamp ends in a zero. Chain verification is POST
// /admin/audit/verify, which recomputes in SQL against the table. Escaping the
// CSV therefore breaks nothing that worked.
func Cell(s string) string {
	if s == "" {
		return s
	}
	if strings.ContainsRune(dangerous, rune(s[0])) {
		return "'" + s
	}
	return s
}

// Row applies Cell to every element, in place, and returns the slice so it can
// be written straight to a csv.Writer.
func Row(cells []string) []string {
	for i := range cells {
		cells[i] = Cell(cells[i])
	}
	return cells
}
