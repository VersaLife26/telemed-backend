package user

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// storedDigestShape is the form the database CHECK constraint accepts
// (migrations/000003_nic_keyed_hash.up.sql). A bcrypt digest starts "$2a$" and
// fails it, which is the structural half of this fix: the column itself now
// refuses the old primitive.
var storedDigestShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func testHasher(t *testing.T, pepper string) *NICHasher {
	t.Helper()
	h, err := NewNICHasher(pepper)
	if err != nil {
		t.Fatalf("NewNICHasher: %v", err)
	}
	return h
}

const (
	// 32 bytes exactly -- the minimum. testNICPepper is also what the
	// integration tests wire into NewService, so a digest written by one is
	// readable by the other.
	testNICPepper      = "0123456789abcdef0123456789abcdef"
	testNICPepperOther = "fedcba9876543210fedcba9876543210"
)

func TestNICHasher_RejectsAPepperTooShortToBeWorthHaving(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"", "short", strings.Repeat("x", NICPepperMinBytes-1)} {
		if _, err := NewNICHasher(p); err == nil {
			t.Fatalf("a %d-byte pepper was accepted; a weak key that looks configured is worse than none",
				len(p))
		}
	}
	if _, err := NewNICHasher(strings.Repeat("x", NICPepperMinBytes)); err != nil {
		t.Fatalf("a %d-byte pepper was refused: %v", NICPepperMinBytes, err)
	}
}

// The property bcrypt could not provide. Every use of nic_hash is an equality
// question, and a randomly salted digest cannot answer one -- which is why one
// NIC could previously register unlimited dependants.
func TestNICHash_IsDeterministicSoDuplicatesAreDetectable(t *testing.T) {
	t.Parallel()

	h := testHasher(t, testNICPepper)
	const nic = "199336501234"

	first := h.Hash(nic)
	if first == "" {
		t.Fatal("empty digest for a valid NIC")
	}
	for i := range 8 {
		if got := h.Hash(nic); got != first {
			t.Fatalf("hash %d differs from hash 0 (%q vs %q): the same NIC must produce the same digest, "+
				"or duplicate detection is impossible", i, got, first)
		}
	}

	// And a different NIC must not collide with it.
	if h.Hash("199336501235") == first {
		t.Fatal("two different NICs produced the same digest")
	}
}

func TestNICHash_HasTheShapeTheDatabaseWillAccept(t *testing.T) {
	t.Parallel()

	h := testHasher(t, testNICPepper)
	for _, nic := range []string{"199336501234", "861234567V", "902345678X"} {
		got := h.Hash(nic)
		if !storedDigestShape.MatchString(got) {
			t.Fatalf("Hash(%q) = %q, which the users_nic_hash_is_keyed_digest CHECK constraint rejects; "+
				"want 64 lowercase hex characters", nic, got)
		}
	}
}

// The pepper is the whole control. If the digest did not depend on it, holding
// the database would be holding the NICs.
func TestNICHash_DependsOnThePepper(t *testing.T) {
	t.Parallel()

	const nic = "199336501234"
	a := testHasher(t, testNICPepper).Hash(nic)
	b := testHasher(t, testNICPepperOther).Hash(nic)

	if a == b {
		t.Fatal("the digest is identical under two different peppers: the key is not being used")
	}
}

// This is the finding, executed. An attacker with a database dump holds the
// digest, the plaintext dob in the same row, and this source file. The NIC
// encodes its own birth date, so everything except a 5-digit serial is already
// known to them.
//
// Under bcrypt that is ~10^5 candidates against a verification function that
// needs no secret -- seconds on a GPU, per row. Under a keyed HMAC the
// candidate set is just as small and just as enumerable, and it does not
// matter: they cannot compute the function at all.
func TestNICHash_SurvivesTheBruteForceTheDateOfBirthEnables(t *testing.T) {
	t.Parallel()

	// A dependant born on day 365 of 1993, as stored in family_members.dob.
	const (
		year   = 1993
		day    = 365
		serial = 1234
	)
	trueNIC := fmt.Sprintf("%04d%03d%05d", year, day, serial)

	legit := testHasher(t, testNICPepper)
	stored := legit.Hash(trueNIC)

	// Sanity: the scheme has to actually work before "it resists attack" means
	// anything. A hash that matches nothing would pass the loop below for
	// entirely the wrong reason.
	if !legit.Matches(trueNIC, stored, legit.Version()) {
		t.Fatal("the real hasher does not recognise its own digest")
	}

	// The attacker. They know the algorithm and the date; they do not have the
	// pepper, which lives in the environment or a KMS and is not in the dump.
	attacker := testHasher(t, testNICPepperOther)

	// The date pins the first seven digits, so this walks the ENTIRE remaining
	// search space, including the correct answer.
	for candidate := range 100000 {
		guess := fmt.Sprintf("%04d%03d%05d", year, day, candidate)
		if attacker.Hash(guess) == stored {
			t.Fatalf("recovered the NIC %q from the digest and the date of birth alone", guess)
		}
	}
}

func TestNormalizeNIC_FoldsHowAHumanTypesIt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{
			name:  "new format with separators the keypad or the user inserts",
			input: []string{"199336501234", "1993 3650 1234", "1993-3650-1234", " 199336501234 "},
			want:  "199336501234",
		},
		{
			name:  "old format, and its check letter in either case",
			input: []string{"861234567V", "861234567v", "86123 4567 v", "86-1234567-V"},
			want:  "861234567V",
		},
		{
			name:  "old format ending X",
			input: []string{"902345678X", "902345678x"},
			want:  "902345678X",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testHasher(t, testNICPepper)
			want := h.Hash(tc.want)

			for _, in := range tc.input {
				if got := NormalizeNIC(in); got != tc.want {
					t.Errorf("NormalizeNIC(%q) = %q, want %q", in, got, tc.want)
				}
				// The normalisation is only worth having if it reaches the
				// digest: without it, one card would be several identities and
				// duplicate detection would silently catch nothing.
				if got := h.Hash(in); got != want {
					t.Errorf("Hash(%q) != Hash(%q): the same card hashed to two different identities", in, tc.want)
				}
			}
		})
	}
}

func TestNICHash_EmptyInputIsNotADigest(t *testing.T) {
	t.Parallel()

	h := testHasher(t, testNICPepper)
	for _, in := range []string{"", "   ", "---", " - "} {
		if got := h.Hash(in); got != "" {
			t.Fatalf("Hash(%q) = %q; hashing an absent NIC would give every row without one the same "+
				"well-formed digest, and they would all read as duplicates of each other", in, got)
		}
		if h.Matches(in, h.Hash("199336501234"), h.Version()) {
			t.Fatalf("Matches(%q, <a real digest>) reported true", in)
		}
	}
}

func TestNICHash_MatchesIsAnEqualityCheckOnTheStoredValue(t *testing.T) {
	t.Parallel()

	h := testHasher(t, testNICPepper)
	stored := h.Hash("861234567V")

	if !h.Matches("861234567v", stored, h.Version()) {
		t.Fatal("a NIC re-entered in lower case did not confirm against its own stored digest")
	}
	if h.Matches("861234568V", stored, h.Version()) {
		t.Fatal("a different NIC confirmed against the stored digest")
	}
	if h.Matches("861234567V", "", h.Version()) {
		t.Fatal("a NIC confirmed against an absent digest")
	}

	// A digest written under a different pepper must not confirm either --
	// this is what a rotation looks like from the inside, and it must fail
	// closed rather than silently accept.
	other := testHasher(t, testNICPepperOther)
	if h.Matches("861234567V", other.Hash("861234567V"), h.Version()) {
		t.Fatal("a digest from another pepper was accepted")
	}
}
