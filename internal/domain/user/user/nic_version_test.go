package user

import (
	"strings"
	"testing"
)

const (
	pepperV1 = "0123456789abcdef0123456789abcdef" // 32 bytes
	pepperV2 = "fedcba9876543210fedcba9876543210"
)

// TestNICRotation_ADigestWrittenUnderTheOldPepperStillVerifies is the whole
// reason nic_hash_version exists.
//
// Before it, rotating NIC_HASH_PEPPER changed the function and every stored
// digest stopped matching -- silently. Duplicate detection would answer "no
// duplicate" and a NIC confirmation would answer "does not match", and both of
// those are the LEGITIMATE answers to those questions, so nothing errored and
// nothing logged. There was no query that could even count the affected rows.
//
// With the version stored on the row, the old pepper stays available for the
// rotation window and the row keeps verifying against the key that actually
// produced it.
func TestNICRotation_ADigestWrittenUnderTheOldPepperStillVerifies(t *testing.T) {
	const nic = "199012345678"

	old, err := NewVersionedNICHasher(1, pepperV1, nil)
	if err != nil {
		t.Fatalf("v1 hasher: %v", err)
	}
	stored, storedVersion := old.Hash(nic), old.Version()

	// The rotation: a new current pepper, the old one retained for verification.
	rotated, err := NewVersionedNICHasher(2, pepperV2, map[int]string{1: pepperV1})
	if err != nil {
		t.Fatalf("v2 hasher: %v", err)
	}

	if !rotated.Matches(nic, stored, storedVersion) {
		t.Fatal("a row written under pepper v1 stopped verifying after rotating to v2")
	}
	// And the wrong NIC still does not match under the old pepper.
	if rotated.Matches("199012345679", stored, storedVersion) {
		t.Fatal("a different NIC matched a v1 digest")
	}

	// New writes go out under the new version, so the rotation makes progress.
	if rotated.Version() != 2 {
		t.Fatalf("Version() = %d, want 2: new digests must be stamped with the new generation", rotated.Version())
	}
	if rotated.Hash(nic) == stored {
		t.Fatal("the v2 digest equals the v1 digest; the pepper did not actually change the function")
	}
}

// TestNICRotation_WithoutTheOldPepperTheAnswerIsUnknownNotNoMatch is the
// distinction the version column buys that a boolean cannot.
//
// A row whose pepper generation this deployment no longer holds is not a row
// whose NIC failed to match. Collapsing the two is precisely what the
// unversioned code did to every row on the day of a rotation, and it is the
// difference between "re-enter your NIC" and "that is not your NIC".
func TestNICRotation_WithoutTheOldPepperTheAnswerIsUnknownNotNoMatch(t *testing.T) {
	const nic = "199012345678"

	old, err := NewVersionedNICHasher(1, pepperV1, nil)
	if err != nil {
		t.Fatalf("v1 hasher: %v", err)
	}
	stored := old.Hash(nic)

	// Rotation completed; the old pepper has been retired.
	rotated, err := NewVersionedNICHasher(2, pepperV2, nil)
	if err != nil {
		t.Fatalf("v2 hasher: %v", err)
	}

	matched, known := rotated.MatchesVersion(nic, stored, 1)
	if known {
		t.Fatal("version 1 reported as evaluable when no v1 pepper is held")
	}
	if matched {
		t.Fatal("matched must be false when the question could not be evaluated")
	}
	// And the collapsing helper still answers false, which is right for an
	// authorisation-shaped caller.
	if rotated.Matches(nic, stored, 1) {
		t.Fatal("Matches must be false for an unknown version")
	}
}

// TestNICHasher_RefusesAnAmbiguousConfiguration guards the invariant that makes
// a version meaningful: one version, one pepper.
func TestNICHasher_RefusesAnAmbiguousConfiguration(t *testing.T) {
	t.Run("previous pepper reusing the current version", func(t *testing.T) {
		_, err := NewVersionedNICHasher(2, pepperV2, map[int]string{2: pepperV1})
		if err == nil {
			t.Fatal("two peppers claiming version 2 were accepted; a digest must identify exactly one pepper")
		}
		if !strings.Contains(err.Error(), "current version") {
			t.Fatalf("error should say why, got: %v", err)
		}
	})
	t.Run("version zero", func(t *testing.T) {
		if _, err := NewVersionedNICHasher(0, pepperV1, nil); err == nil {
			t.Fatal("version 0 accepted; 0 is what an unset integer looks like and must not read as a generation")
		}
	})
	t.Run("short previous pepper", func(t *testing.T) {
		if _, err := NewVersionedNICHasher(2, pepperV2, map[int]string{1: "too-short"}); err == nil {
			t.Fatal("a short retired pepper was accepted")
		}
	})
	t.Run("short current pepper", func(t *testing.T) {
		if _, err := NewVersionedNICHasher(1, "too-short", nil); err == nil {
			t.Fatal("a short pepper was accepted")
		}
	})
}

// TestNICHasher_DefaultConstructorStampsTheCurrentVersion pins that an
// un-rotated deployment writes version 1 -- which is what migration 000004
// backfills onto every existing row.
func TestNICHasher_DefaultConstructorStampsTheCurrentVersion(t *testing.T) {
	h, err := NewNICHasher(pepperV1)
	if err != nil {
		t.Fatalf("NewNICHasher: %v", err)
	}
	if h.Version() != NICPepperVersionCurrent {
		t.Fatalf("Version() = %d, want %d", h.Version(), NICPepperVersionCurrent)
	}
	if NICPepperVersionCurrent != 1 {
		t.Fatalf("NICPepperVersionCurrent = %d, want 1 to match the migration's backfill",
			NICPepperVersionCurrent)
	}
}
