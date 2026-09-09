package user

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Keyed hashing for the National Identity Card number.
//
// # Why not bcrypt
//
// This column was bcrypt, and bcrypt is the wrong primitive here. bcrypt's
// cost factor buys time per guess, which is only worth paying when the guess
// space is large. A Sri Lankan NIC's is not:
//
//   - The old format is YYDDDNNNNC and the new one YYYYDDDNNNNN. Both ENCODE
//     THEIR OWN BIRTH DATE -- YY/YYYY is the year and DDD the day of the year
//     (offset by 500 for women).
//   - family_members stores `dob DATE NOT NULL` in the SAME ROW as nic_hash.
//
// So an attacker holding a database dump already knows every digit of the date
// portion and is left guessing a 4-5 digit serial plus a check character:
// on the order of 10^4-10^5 candidates. At bcrypt.DefaultCost that is seconds
// on a GPU, per row. The per-hash salt does not help -- it defeats a shared
// rainbow table, and the problem here is not table reuse, it is that the
// search space fits in a for-loop.
//
// # Why HMAC-SHA256 with a pepper
//
// The pepper is a secret the database does not contain. Recovering a NIC now
// requires the dump AND a key held in the environment or a KMS, which is
// exactly the separation `BANK_ENCRYPTION_KEY` already relies on for bank
// details. Speed stops being a weakness once the attacker cannot compute the
// function at all.
//
// It is also the right SHAPE for what this column is for. Every use of
// nic_hash is an EQUALITY question -- "is this the NIC already on file", "has
// this NIC been registered before" -- and never "reverse this". A randomly
// salted hash cannot answer the second question at all, which is why one NIC
// could previously register unlimited dependants. A keyed hash is
// deterministic, so equality and duplicate detection are both a plain indexed
// lookup.
//
// The deliberate trade: determinism means two identical NICs produce identical
// digests, so the column reveals *that* two rows share a NIC. That is the
// property being bought on purpose, and it leaks nothing an attacker who can
// read the table does not already learn from `owner_user_id` and `name`.

// NICPepperMinBytes is the minimum accepted pepper length. HMAC-SHA256's
// security ceiling is its 256-bit output, so anything shorter than 32 bytes is
// the weakest link for no saving.
const NICPepperMinBytes = 32

// # Rotation, and why every digest carries a version
//
// A keyed digest is only meaningful next to the key that produced it. Rotating
// NIC_HASH_PEPPER changes the function, so every value already in `nic_hash`
// stops matching -- and it stops matching SILENTLY. Duplicate detection just
// finds nothing; a "confirm the NIC on file" check just says no. Nothing
// errors, nothing logs, and the failure looks exactly like the legitimate
// answer. That is the worst shape a security control can fail in, and the
// column had no way to tell the two apart.
//
// So the version travels with the digest. `nic_hash_version` records which
// pepper generation produced `nic_hash`, and the two are written together or
// not at all -- a database CHECK enforces that (migration 000004), because an
// invariant that lives only in Go survives exactly as long as the next
// refactor.
//
// With the version stored, a rotation is a normal operation rather than a
// silent data loss:
//
//  1. Generate a new pepper. Set NIC_HASH_PEPPER to it and bump
//     NIC_HASH_PEPPER_VERSION. Move the old one to NIC_HASH_PEPPER_PREVIOUS
//     as "<version>:<pepper>".
//  2. New writes get the new version. Reads of older rows still verify,
//     because Matches selects the pepper by the row's own version.
//  3. When every row has been rewritten under the new version -- which the
//     column now makes it possible to *query* -- drop the old pepper.
//
// Without step 2 a rotation is a one-way destruction of every stored digest.
// The version column is what makes step 2 expressible at all, which is why it
// is added before anything depends on the digest rather than after.

// NICPepperVersionCurrent is the version stamped on digests written by a
// deployment that has never rotated. Existing rows were all written under it.
const NICPepperVersionCurrent = 1

// NICHasher computes the keyed digest stored in `nic_hash`.
//
// It is a type rather than a package-level function because the pepper is a
// secret with a lifecycle: it is loaded once at boot, must never be logged,
// and one day has to be rotated. A free function reading a global would make
// all three invisible.
type NICHasher struct {
	version int
	pepper  []byte
	// previous holds superseded peppers by version, for the window in which
	// old rows have not yet been rewritten. Verification only -- nothing is
	// ever WRITTEN under a retired pepper, so a rotation always makes forward
	// progress and can never silently un-rotate a row.
	previous map[int][]byte
}

// NewNICHasher builds a hasher from the configured pepper at the current
// version, with no superseded peppers. This is the shape of a deployment that
// has never rotated.
func NewNICHasher(pepper string) (*NICHasher, error) {
	return NewVersionedNICHasher(NICPepperVersionCurrent, pepper, nil)
}

// NewVersionedNICHasher builds a hasher that writes under version, and can
// still verify digests written under any pepper in previous (keyed by the
// version that produced it).
//
// It refuses a short pepper rather than padding or stretching it. Silently
// accepting a weak key is how a control ends up looking present in code review
// and absent in production. It refuses a non-positive version for the same
// reason the database CHECK does: zero is what an unset integer looks like,
// and "unset" must never be mistaken for "version 0". And it refuses a
// previous pepper claiming the current version, which would make the digest a
// row carries ambiguous -- the one thing the version exists to prevent.
func NewVersionedNICHasher(version int, pepper string, previous map[int]string) (*NICHasher, error) {
	if version <= 0 {
		return nil, fmt.Errorf("user: NIC_HASH_PEPPER_VERSION must be a positive integer, got %d", version)
	}
	if len(pepper) < NICPepperMinBytes {
		return nil, fmt.Errorf("user: NIC_HASH_PEPPER must be at least %d bytes, got %d",
			NICPepperMinBytes, len(pepper))
	}
	h := &NICHasher{version: version, pepper: []byte(pepper)}
	if len(previous) > 0 {
		h.previous = make(map[int][]byte, len(previous))
		for v, p := range previous {
			if v <= 0 {
				return nil, fmt.Errorf("user: NIC_HASH_PEPPER_PREVIOUS has a non-positive version %d", v)
			}
			if v == version {
				return nil, fmt.Errorf("user: NIC_HASH_PEPPER_PREVIOUS reuses the current version %d; a digest must identify exactly one pepper", v)
			}
			if len(p) < NICPepperMinBytes {
				return nil, fmt.Errorf("user: NIC_HASH_PEPPER_PREVIOUS version %d is shorter than %d bytes", v, NICPepperMinBytes)
			}
			h.previous[v] = []byte(p)
		}
	}
	return h, nil
}

// Version is the pepper generation this hasher writes under. Every digest it
// produces must be stored alongside this value.
func (h *NICHasher) Version() int { return h.version }

// pepperFor returns the pepper that produced digests stamped with version.
func (h *NICHasher) pepperFor(version int) ([]byte, bool) {
	if version == h.version {
		return h.pepper, true
	}
	p, ok := h.previous[version]
	return p, ok
}

// GenerateEphemeralNICPepper returns a fresh random pepper.
//
// It exists for development only, mirroring GenerateEphemeralKeyPEM: a
// developer gets a working `make run` with no setup, and the digests written
// under it stop matching after a restart -- which is loud, local, and
// harmless. Production refuses to boot without a real pepper, because a
// rotating one there would silently break duplicate detection and NIC
// confirmation for every row written before the last deploy.
func GenerateEphemeralNICPepper() (string, error) {
	buf := make([]byte, NICPepperMinBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("user: generate ephemeral NIC pepper: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NormalizeNIC folds the cosmetic variations a human types into one canonical
// form, so that equality on the digest means equality on the identity.
//
// Without this the whole scheme quietly fails open: "199012345678" and
// "1990 1234 5678" would be two different people to the database, and
// duplicate detection -- the entire reason for choosing a deterministic hash
// -- would catch nothing. Old-format cards end in a V or an X and are written
// either way, so case is folded too.
//
// Only separators are removed. No attempt is made to validate or reformat the
// number: this service is not the registrar, and a card that does not match
// our idea of the format still belongs to a real patient.
func NormalizeNIC(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		default:
			// spaces, hyphens, non-breaking spaces, anything else: dropped
		}
	}
	return b.String()
}

// Hash returns the stored form of nic: lowercase hex of
// HMAC-SHA256(pepper, NormalizeNIC(nic)).
//
// An empty or separator-only input returns the empty string rather than the
// digest of "", so a caller cannot accidentally store a well-formed-looking
// hash that means "no NIC was given" -- which would then collide with every
// other row that gave no NIC and read as a duplicate.
func (h *NICHasher) Hash(nic string) string {
	return h.hashWith(h.pepper, nic)
}

func (h *NICHasher) hashWith(pepper []byte, nic string) string {
	normalized := NormalizeNIC(nic)
	if normalized == "" {
		return ""
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil))
}

// Matches reports whether nic hashes to stored under the pepper generation
// named by version, in constant time with respect to the digest contents.
//
// A version this process holds no pepper for returns false -- but that is a
// DIFFERENT false from "the NIC does not match", and a caller that needs to
// tell them apart should use MatchesVersion. This one collapses both, which is
// the right answer for an authorisation-shaped question and the wrong one for
// a data-migration-shaped question.
//
// hmac.Equal rather than ==: the comparison is against a value an attacker can
// influence by choosing what NIC to submit, and a byte-wise early return is a
// (small, but free to remove) oracle on the stored digest.
func (h *NICHasher) Matches(nic, stored string, version int) bool {
	ok, _ := h.MatchesVersion(nic, stored, version)
	return ok
}

// MatchesVersion is Matches, plus whether this process could evaluate the
// question at all.
//
// known is false when the row was written under a pepper generation this
// deployment no longer holds. That is an operational fact -- a rotation
// completed before every row was rewritten -- and it must not be reported as
// "the NIC does not match", which is what the unversioned code did to every
// row on the day of a rotation.
func (h *NICHasher) MatchesVersion(nic, stored string, version int) (matched, known bool) {
	pepper, known := h.pepperFor(version)
	if !known {
		return false, false
	}
	if stored == "" {
		return false, true
	}
	computed := h.hashWith(pepper, nic)
	if computed == "" {
		return false, true
	}
	return hmac.Equal([]byte(computed), []byte(stored)), true
}
