package disputes

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	mw "telemed/internal/platform/middleware"
)

// disputes.description is up to 4000 characters a patient typed about their
// own care, and "quality_of_care" is one of the five categories. It is the one
// concrete counter-example to the platform's claim that an administrator never
// sees clinical data -- a complaint about a consultation contains the
// consultation.
//
// It cannot simply be dropped the way the cancellation reason was (F20d):
// nobody can adjudicate a quality-of-care complaint without reading it. So the
// rule is that reading it is narrow, deliberate and recorded -- and this is
// that rule, tested directly, because the DTO and the audit entry both derive
// from it.
func TestMayReadDescription(t *testing.T) {
	t.Parallel()

	holder := uuid.New()
	other := uuid.New()
	assigned := func(id uuid.UUID) Dispute { return Dispute{AssignedTo: &id} }
	unassigned := Dispute{}

	principal := func(role mw.Role) mw.Principal { return mw.Principal{Roles: []mw.Role{role}} }

	cases := []struct {
		name    string
		dispute Dispute
		role    mw.Role
		actor   Actor
		want    bool
	}{
		{"support working an unassigned dispute", unassigned, mw.RoleSupport, Actor{ID: holder}, true},
		{"support working their own dispute", assigned(holder), mw.RoleSupport, Actor{ID: holder}, true},
		{"admin working their own dispute", assigned(holder), mw.RoleAdmin, Actor{ID: holder}, true},
		{"super_admin overriding", assigned(holder), mw.RoleSuperAdmin, Actor{ID: other, CanForce: true}, true},

		{"support reading a colleague's dispute", assigned(holder), mw.RoleSupport, Actor{ID: other}, false},
		{"admin reading a colleague's dispute", assigned(holder), mw.RoleAdmin, Actor{ID: other}, false},

		// finance needs the refund amount and the category. ops needs the
		// queue. Neither needs the patient's narrative.
		{"finance, even unassigned", unassigned, mw.RoleFinance, Actor{ID: holder}, false},
		{"finance, even assigned to them", assigned(holder), mw.RoleFinance, Actor{ID: holder}, false},
		{"ops, even unassigned", unassigned, mw.RoleOps, Actor{ID: holder}, false},

		// And never a non-admin role, whatever else is true.
		{"a patient token", unassigned, mw.RolePatient, Actor{ID: holder}, false},
		{"a doctor token", unassigned, mw.RoleDoctor, Actor{ID: holder}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mayReadDescription(tc.dispute, principal(tc.role), tc.actor)
			require.Equal(t, tc.want, got,
				"role %q, assigned=%v, actor=%v", tc.role, tc.dispute.AssignedTo, tc.actor)
		})
	}
}

// The list DTO must never carry the narrative. A queue view is browsing, and
// browsing 25 patients' complaints at a time is not adjudication.
func TestListDTOOmitsTheDescription(t *testing.T) {
	t.Parallel()

	d := Dispute{
		ID:          uuid.New(),
		Category:    "quality_of_care",
		Description: "the doctor dismissed my chest pain and told me it was anxiety",
	}
	require.Empty(t, toDTO(d).Description,
		"a patient's account of their care was rendered into a list response")
	require.Equal(t, "quality_of_care", toDTO(d).Category,
		"the category must survive -- it is what a queue is triaged on")
}

// An admin role is only an admin role from the admin issuer; the service layer
// never passes through RequireRole.
func TestDescriptionIsNotReadableByAForgedAdmin(t *testing.T) {
	mw.SetAdminIssuer("https://auth.yourapp.lk/realms/telemedicine")
	t.Cleanup(func() { mw.SetAdminIssuer("") })

	forged := mw.Principal{Roles: []mw.Role{mw.RoleSupport}, Issuer: "telemed-user-service"}
	require.False(t, mayReadDescription(Dispute{}, forged, Actor{ID: uuid.New()}),
		"a support role asserted by the patient issuer read a patient's complaint")

	genuine := mw.Principal{Roles: []mw.Role{mw.RoleSupport}, Issuer: "https://auth.yourapp.lk/realms/telemedicine"}
	require.True(t, mayReadDescription(Dispute{}, genuine, Actor{ID: uuid.New()}))
}
