package auth

import "testing"

func TestRolePermissionMatrix(t *testing.T) {
	cases := []struct {
		role    string
		allowed string
		denied  string
	}{
		{"user", PermissionAccountWrite, PermissionAdminUsersRead},
		{"support_admin", PermissionAdminUsersRead, PermissionAdminUsersManage},
		{"content_admin", PermissionAdminPricing, PermissionAdminMemberships},
		{"membership_admin", PermissionAdminMemberships, PermissionAdminReleases},
		{"release_admin", PermissionAdminReleases, PermissionAdminPricing},
		{"super_admin", PermissionAdminAuditRead, "admin.unknown"},
	}
	for _, test := range cases {
		t.Run(test.role, func(t *testing.T) {
			user := User{Roles: []string{test.role}}
			if !HasPermission(user, test.allowed) {
				t.Fatalf("role %s missing %s", test.role, test.allowed)
			}
			if HasPermission(user, test.denied) {
				t.Fatalf("role %s unexpectedly has %s", test.role, test.denied)
			}
		})
	}
}

func TestMembershipAdministratorCanReadUsersWithoutManagingAccounts(t *testing.T) {
	user := User{Roles: []string{"membership_admin"}}
	if !HasPermission(user, PermissionAdminUsersRead) {
		t.Fatal("membership administrator cannot find accounts to adjust")
	}
	if HasPermission(user, PermissionAdminUsersManage) {
		t.Fatal("membership administrator can create or disable accounts")
	}
}
