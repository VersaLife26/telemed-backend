package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The audit export is the file handed to a regulator, and several of its
// columns carry text somebody else authored: a dispute description a patient
// typed, a doctor rejection reason, a config value, the requesting client's
// User-Agent. A cell beginning "=", "+", "-", "@", TAB or CR is evaluated as a
// formula when the file is opened, so a low-privilege admin who can get a
// string into any audited field gets code execution on the auditor's machine,
// out of a file this platform vouched for.
func TestExportRow_NeutralisesFormulaInjection(t *testing.T) {
	t.Parallel()

	const payload = `=cmd|'/c calc'!A1`
	e := Entry{
		ID:           42,
		ActorID:      uuid.New(),
		ActorRole:    "support",
		Action:       "dispute.resolved",
		ResourceType: "dispute",
		ResourceID:   uuid.NewString(),
		OldValue:     []byte(`{"status":"open"}`),
		NewValue:     []byte(payload),
		IP:           "203.0.113.9",
		UserAgent:    payload,
		RequestID:    "req-1",
		CreatedAt:    time.Date(2026, 8, 20, 9, 15, 0, 0, time.UTC),
		PrevHash:     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		RowHash:      "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03",
	}

	row := exportRow(e)

	// new_value (index 7) and user_agent (index 9) both carried the payload.
	for _, i := range []int{7, 9} {
		if strings.HasPrefix(row[i], "=") {
			t.Fatalf("column %d starts a formula: %q", i, row[i])
		}
		if row[i] != "'"+payload {
			t.Fatalf("column %d = %q, want the value with a leading apostrophe", i, row[i])
		}
	}

	// The chain columns must survive byte for byte -- they are what the file
	// exists to carry.
	if row[12] != e.PrevHash || row[13] != e.RowHash {
		t.Fatalf("the hash chain columns were altered: prev=%q row=%q", row[12], row[13])
	}
	if row[0] != "42" || row[1] != e.ActorID.String() {
		t.Fatalf("identity columns altered: %q %q", row[0], row[1])
	}
	if row[3] != "dispute.resolved" {
		t.Fatalf("an ordinary value was altered: %q", row[3])
	}
}

// A leading "-" and a leading "@" are the two that implementations of this
// mitigation most often miss, and "+94..." is what a Sri Lankan phone number
// in a new_value blob looks like.
func TestExportRow_CoversEveryDangerousLead(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`-2+3+cmd|'/c calc'!A1`,
		`@SUM(1+1)*cmd|'/c calc'!A1`,
		`+94771234567`,
		"\t=1+1",
		"\r=1+1",
	} {
		row := exportRow(Entry{NewValue: []byte(payload), CreatedAt: time.Now().UTC()})
		if row[7] != "'"+payload {
			t.Fatalf("payload %q rendered as %q; it must be prefixed", payload, row[7])
		}
	}
}
