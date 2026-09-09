package rbac

import (
	"sort"

	mw "telemed/internal/platform/middleware"
)

// Descriptions gives each group a human label and a sentence explaining what
// the group actually reaches. The console renders these directly, so this is
// the text a super_admin reads while deciding what a colleague should be able
// to do -- it is worth more care than a variable name.
//
// Anything NOT in Matrix cannot be granted at all, however senior the account:
// the clinical surfaces (documents, prescriptions, clinical notes, recording
// content) are denied to every admin role in record-service and
// consultation-service, not merely unlisted here. See security review F3/F4.
var Descriptions = map[Group]struct{ Label, Detail string }{
	GroupCredentialing: {"Doctor credentialing",
		"Review and verify doctor registrations: SLMC number, certificates and the verification checklist."},
	GroupAdminUsers: {"Admin accounts",
		"Create admin accounts, change their role, and deactivate them. Super admin only -- any lesser role able to grant itself access would be a privilege-escalation hole."},
	GroupUsers: {"Patients",
		"View patient accounts, suspend and reinstate them. Account state only: this does NOT include medical records."},
	GroupAppointments: {"Appointments",
		"View bookings, force-cancel an appointment, and resolve a double booking."},
	GroupFinance: {"Finance",
		"Issue refunds, run payouts, read the ledger and set commission rules. Finance or super admin only -- moving money is not part of general admin work."},
	GroupContent: {"Content",
		"Manage the drug catalogue, specialties, symptoms, districts and health articles."},
	GroupDisputes: {"Disputes",
		"Assign, comment on and resolve patient disputes."},
	GroupConfig: {"Platform settings",
		"Feature flags, cancellation policy, fee caps and commission configuration. Support is excluded because this sets financial and operational policy."},
	GroupAnalytics: {"Analytics",
		"Revenue and utilisation reporting, in aggregate."},
	GroupAudit: {"Audit log",
		"Read a filtered, paginated view of the audit trail."},
	GroupAuditExport: {"Audit export",
		"Download the ENTIRE audit trail as a file. Deliberately narrower than reading it: exfiltrating the whole log is the move that precedes tampering with it."},
}

// GroupPermission is one row of the permission grid the console renders.
type GroupPermission struct {
	Group  Group     `json:"group"`
	Label  string    `json:"label"`
	Detail string    `json:"detail"`
	Roles  []mw.Role `json:"roles"`
}

// Describe returns the whole matrix in a stable order, so the console shows
// what this service ACTUALLY enforces rather than a second copy of the table
// maintained by hand in another repository. The console keeps its own
// synchronous copy for edge-time route guarding, but what a super_admin is
// shown while changing someone's access comes from here -- a UI that displays
// a permission the server does not grant (or hides one it does) is worse than
// no UI, because it is confidently wrong.
func Describe() []GroupPermission {
	out := make([]GroupPermission, 0, len(AllGroups))
	for _, g := range AllGroups {
		d := Descriptions[g]
		roles := Roles(g)
		// Copy: Roles may hand back the package-level slice (mw.AdminRoles),
		// and a caller sorting the result in place would silently reorder the
		// live authorization table.
		rs := make([]mw.Role, len(roles))
		copy(rs, roles)
		sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
		out = append(out, GroupPermission{Group: g, Label: d.Label, Detail: d.Detail, Roles: rs})
	}
	return out
}

// RolesFor returns every group r may reach, for "what can this admin do".
func RolesFor(r mw.Role) []Group {
	var out []Group
	for _, g := range AllGroups {
		for _, allowed := range Roles(g) {
			if allowed == r {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// AssignableRoles is the set a super_admin may hand out, in the order the
// console should offer them: least privilege first, so the default reading
// order is not "start from the most powerful".
//
// It is deliberately NOT mw.AdminRoles: that slice is the "any admin role"
// list used for authorization checks, and reusing it here would mean a role
// added for one purpose silently becomes assignable in the console.
func AssignableRoles() []mw.Role {
	return []mw.Role{mw.RoleSupport, mw.RoleOps, mw.RoleFinance, mw.RoleAdmin, mw.RoleSuperAdmin}
}
