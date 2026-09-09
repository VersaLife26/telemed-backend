//go:build integration

package user

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The Go-side half of the NIC fix is in nic_test.go. This file asserts the
// half that outlives it: what the DATABASE will and will not accept.
//
// A comment explaining that bcrypt is the wrong primitive stops being true the
// moment someone changes the code without reading it. The CHECK constraint
// added by migrations/000003_nic_keyed_hash.up.sql does not have that failure
// mode.

// TestIntegration_NICHashColumnRefusesABcryptDigest is the structural fix.
// The exact value the old code wrote is now rejected by Postgres itself.
func TestIntegration_NICHashColumnRefusesABcryptDigest(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	owner := mustCreateUser(t, ctx, svc.repo, "+94774444444")

	// Exactly what hashNIC used to produce, computed the same way.
	legacy, err := bcrypt.GenerateFromPassword([]byte("199336501234"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"the bcrypt digest the old code wrote", string(legacy)},
		{"a bcrypt digest with the 2b prefix", "$2b$10$" + strings.Repeat("a", 53)},
		{"a plaintext NIC", "199336501234"},
		{"a sha256 digest in UPPERCASE hex", strings.Repeat("AB", 32)},
		{"63 hex characters", strings.Repeat("a", 63)},
		{"65 hex characters", strings.Repeat("a", 65)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A version is supplied so the row fails on the digest-SHAPE
			// constraint this test is about, rather than on 000004's
			// digest-and-version pair constraint. Otherwise every case here
			// would pass for the wrong reason.
			_, err := pool.Exec(ctx,
				`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
				 VALUES ($1, 'Constraint Probe', '2018-05-01', 'child', $2, 1)`,
				owner.ID, tc.value)
			if err == nil {
				t.Fatalf("Postgres accepted %q into nic_hash; the column must refuse anything "+
					"that is not a 64-character lowercase hex digest, so a future refactor "+
					"cannot reintroduce a brute-forceable hash", tc.value)
			}
			if !strings.Contains(err.Error(), "nic_hash_is_keyed_digest") {
				t.Fatalf("insert failed for the wrong reason: %v", err)
			}
		})
	}

	// Positive control: the value the current code produces is accepted. A
	// constraint that rejected everything would pass every case above and be
	// worthless.
	t.Run("the keyed digest the current code writes", func(t *testing.T) {
		h := testHasher(t, testNICPepper)
		// nic_hash_version is supplied because migration 000004 requires the
		// digest and its key generation to travel together. A digest written
		// without one is unverifiable after any rotation, so the database
		// refuses it -- see TestIntegration_NICHashVersionTravelsWithTheDigest.
		_, err := pool.Exec(ctx,
			`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
			 VALUES ($1, 'Constraint Probe OK', '2018-05-01', 'child', $2, $3)`,
			owner.ID, h.Hash("199336501234"), h.Version())
		if err != nil {
			t.Fatalf("the current digest was rejected: %v", err)
		}
	})

	// NULL stays legal: nic_hash is optional, and the up-migration nulls every
	// legacy row precisely because a bcrypt digest cannot be converted.
	t.Run("NULL is still permitted", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash)
			 VALUES ($1, 'No NIC', '2018-05-01', 'child', NULL)`, owner.ID)
		if err != nil {
			t.Fatalf("a NULL nic_hash was rejected: %v", err)
		}
	})
}

// TestIntegration_TheSameNICStoresTheSameDigest walks the whole path -- service
// to repository to Postgres and back -- and asserts the property duplicate
// detection needs. Under bcrypt these two rows carried different values and
// there was no query that could tell they were the same person.
func TestIntegration_TheSameNICStoresTheSameDigest(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	mother := mustCreateUser(t, ctx, svc.repo, "+94775555555")
	father := mustCreateUser(t, ctx, svc.repo, "+94776666666")

	dob := time.Date(2018, 5, 1, 0, 0, 0, 0, time.UTC)

	// The same child, added by each parent, with the NIC typed differently --
	// which is what actually happens on two phones.
	a, err := svc.AddFamilyMember(ctx, mother.ID, FamilyMemberInput{
		Name: "Child", DOB: dob, Relation: RelationChild, NIC: "199336501234",
	})
	if err != nil {
		t.Fatalf("AddFamilyMember (mother): %v", err)
	}
	b, err := svc.AddFamilyMember(ctx, father.ID, FamilyMemberInput{
		Name: "Child", DOB: dob, Relation: RelationChild, NIC: "1993 3650 1234",
	})
	if err != nil {
		t.Fatalf("AddFamilyMember (father): %v", err)
	}

	if a.NICHash == nil || b.NICHash == nil {
		t.Fatal("a NIC was supplied and no digest was stored")
	}
	if *a.NICHash != *b.NICHash {
		t.Fatalf("the same NIC stored two different digests (%q vs %q): duplicate detection is impossible",
			*a.NICHash, *b.NICHash)
	}

	// And the digest is findable by equality, which is the only reason to make
	// it deterministic in the first place.
	var found int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM family_members WHERE nic_hash = $1 AND deleted_at IS NULL`,
		*a.NICHash).Scan(&found); err != nil {
		t.Fatalf("lookup by digest: %v", err)
	}
	if found != 2 {
		t.Fatalf("lookup by nic_hash found %d rows, want 2", found)
	}

	// The plaintext NIC must not be anywhere in the row.
	var name, relation string
	if err := pool.QueryRow(ctx,
		`SELECT name, relation FROM family_members WHERE id = $1`, a.ID).Scan(&name, &relation); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(name+relation, "199336501234") {
		t.Fatal("the plaintext NIC was stored alongside its digest")
	}
}

// TestIntegration_NICHashVersionTravelsWithTheDigest asserts the invariant that
// migration 000004 exists to enforce, at the only layer where enforcing it
// survives a refactor.
//
// A digest with no version is a value nobody can ever verify again: the pepper
// that produced it is unidentifiable, so after any rotation the row is
// indistinguishable from one whose NIC simply does not match. A version with no
// digest is a claim about an empty column. The database refuses both, which is
// why the rule outlives the Go code that currently honours it.
func TestIntegration_NICHashVersionTravelsWithTheDigest(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newTestService(t, pool)

	owner := mustCreateUser(t, ctx, svc.repo, "+94775555511")
	digest := strings.Repeat("a", 64)

	t.Run("a digest with no version is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
			 VALUES ($1, 'Child', '2015-01-01', 'child', $2, NULL)`, owner.ID, digest)
		if err == nil {
			t.Fatal("a nic_hash with a NULL nic_hash_version was accepted: after a rotation nothing can verify it")
		}
		if !strings.Contains(err.Error(), "nic_hash_version_travels_with_digest") {
			t.Fatalf("rejected by the wrong constraint: %v", err)
		}
	})

	t.Run("a version with no digest is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
			 VALUES ($1, 'Child', '2015-01-01', 'child', NULL, 1)`, owner.ID)
		if err == nil {
			t.Fatal("a nic_hash_version with a NULL nic_hash was accepted")
		}
	})

	t.Run("version zero is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO family_members (owner_user_id, name, dob, relation, nic_hash, nic_hash_version)
			 VALUES ($1, 'Child', '2015-01-01', 'child', $2, 0)`, owner.ID, digest)
		if err == nil {
			t.Fatal("version 0 was accepted: 0 is what an unset integer looks like and must not read as a generation")
		}
	})

	t.Run("the pair is accepted and round-trips", func(t *testing.T) {
		member, err := svc.AddFamilyMember(ctx, owner.ID, FamilyMemberInput{
			Name: "Child", DOB: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC),
			Relation: RelationChild, NIC: "201512345678",
		})
		if err != nil {
			t.Fatalf("add family member: %v", err)
		}
		if member.NICHash == nil || member.NICHashVersion == nil {
			t.Fatalf("service wrote digest=%v version=%v; both or neither", member.NICHash, member.NICHashVersion)
		}
		if *member.NICHashVersion != svc.nic.Version() {
			t.Fatalf("stored version %d, hasher writes %d", *member.NICHashVersion, svc.nic.Version())
		}

		reread, err := svc.repo.GetFamilyMember(ctx, pool, member.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if reread.NICHashVersion == nil || *reread.NICHashVersion != *member.NICHashVersion {
			t.Fatalf("version did not survive the round trip: %v", reread.NICHashVersion)
		}
	})

	t.Run("an edit that keeps the NIC carries the version forward", func(t *testing.T) {
		member, err := svc.AddFamilyMember(ctx, owner.ID, FamilyMemberInput{
			Name: "Second", DOB: time.Date(2016, 2, 2, 0, 0, 0, 0, time.UTC),
			Relation: RelationChild, NIC: "201612345678",
		})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		// No NIC in the update: the existing digest AND its version must both
		// be carried forward, or the CHECK rejects the row.
		updated, err := svc.UpdateFamilyMember(ctx, owner.ID, member.ID, FamilyMemberInput{
			Name: "Second Renamed", DOB: member.DOB, Relation: RelationChild,
		}, member.Version)
		if err != nil {
			t.Fatalf("update without a NIC: %v", err)
		}
		if updated.NICHashVersion == nil {
			t.Fatal("the version was dropped by an edit that kept the digest")
		}
	})
}
