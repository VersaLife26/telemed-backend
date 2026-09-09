package rbac

import (
	"testing"

	mw "telemed/internal/platform/middleware"
)

// TestEveryGroupIsDescribed fails when a group is added to AllGroups without
// telling a super_admin what granting it means. An undescribed row in the
// permission grid is a blank cell next to a checkbox that changes who can
// reach patient data.
func TestEveryGroupIsDescribed(t *testing.T) {
	for _, g := range AllGroups {
		d, ok := Descriptions[g]
		if !ok {
			t.Errorf("group %q has no description; add one to Descriptions", g)
			continue
		}
		if d.Label == "" || d.Detail == "" {
			t.Errorf("group %q has an empty label or detail", g)
		}
	}
	if len(Descriptions) != len(AllGroups) {
		t.Errorf("Descriptions has %d entries, AllGroups has %d: a description "+
			"for a group that is not mounted describes access nobody has",
			len(Descriptions), len(AllGroups))
	}
}

// TestDescribeMatchesMatrix pins Describe to the table main.go actually
// mounts. The whole point of the endpoint is that the console stops showing a
// second, hand-maintained copy of the permissions, so it has to be derived
// from Matrix rather than restating it.
func TestDescribeMatchesMatrix(t *testing.T) {
	got := Describe()
	if len(got) != len(AllGroups) {
		t.Fatalf("Describe returned %d groups, want %d", len(got), len(AllGroups))
	}
	for _, row := range got {
		want := Roles(row.Group)
		if len(row.Roles) != len(want) {
			t.Errorf("%s: described %d roles, matrix has %d", row.Group, len(row.Roles), len(want))
			continue
		}
		for _, r := range want {
			found := false
			for _, d := range row.Roles {
				if d == r {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: matrix allows %q, description omits it", row.Group, r)
			}
		}
	}
}

// TestDescribeDoesNotMutateTheMatrix guards a genuine footgun: Roles() hands
// back the package-level mw.AdminRoles slice for most groups, so sorting the
// result in place would reorder the live authorization table for every other
// caller in the process.
func TestDescribeDoesNotMutateTheMatrix(t *testing.T) {
	before := make([]mw.Role, len(mw.AdminRoles))
	copy(before, mw.AdminRoles)

	_ = Describe()

	for i := range before {
		if mw.AdminRoles[i] != before[i] {
			t.Fatalf("Describe() reordered mw.AdminRoles: index %d is %q, was %q",
				i, mw.AdminRoles[i], before[i])
		}
	}
}

// TestAdminUsersIsSuperAdminOnly states the invariant the whole feature rests
// on in its own test, so a widening shows up as this test failing by name.
func TestAdminUsersIsSuperAdminOnly(t *testing.T) {
	roles := Roles(GroupAdminUsers)
	if len(roles) != 1 || roles[0] != mw.RoleSuperAdmin {
		t.Fatalf("admin_users is reachable by %v; it must be super_admin alone, "+
			"or a lesser admin can grant itself or a peer more access", roles)
	}
}

// TestAssignableRolesMatchTheDatabaseConstraint pins the console's role picker
// to what admin_users.role actually accepts. The column carries
// CHECK (role IN ('admin','super_admin','ops','finance','support')), so
// offering a role outside that set produces a 500 at save time, and omitting
// one silently makes a legitimate role unassignable through the UI.
func TestAssignableRolesMatchTheDatabaseConstraint(t *testing.T) {
	// Mirrors migrations/000003_admin_core.up.sql.
	constraint := map[mw.Role]bool{
		mw.RoleAdmin: true, mw.RoleSuperAdmin: true, mw.RoleOps: true,
		mw.RoleFinance: true, mw.RoleSupport: true,
	}

	assignable := AssignableRoles()
	if len(assignable) != len(constraint) {
		t.Fatalf("AssignableRoles has %d roles, the CHECK constraint permits %d",
			len(assignable), len(constraint))
	}
	seen := map[mw.Role]bool{}
	for _, r := range assignable {
		if !constraint[r] {
			t.Errorf("role %q is offered by the console but rejected by the "+
				"admin_users.role CHECK constraint", r)
		}
		if seen[r] {
			t.Errorf("role %q is listed twice", r)
		}
		seen[r] = true
	}
	for r := range constraint {
		if !seen[r] {
			t.Errorf("role %q is a valid admin_users.role but cannot be "+
				"assigned through the console", r)
		}
	}
}

// TestEveryAssignableRoleGrantsSomething catches a role that can be handed out
// but reaches nothing -- an account that looks provisioned and cannot work.
func TestEveryAssignableRoleGrantsSomething(t *testing.T) {
	for _, r := range AssignableRoles() {
		if groups := RolesFor(r); len(groups) == 0 {
			t.Errorf("role %q is assignable but appears in no group in Matrix", r)
		}
	}
}
