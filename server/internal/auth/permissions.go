package auth

import "sort"

const (
	PermissionAccountRead        = "account.read"
	PermissionAccountWrite       = "account.write"
	PermissionMembershipRedeem   = "membership.redeem"
	PermissionAdminUsersRead     = "admin.users.read"
	PermissionAdminUsersManage   = "admin.users.manage"
	PermissionAdminPricing       = "admin.pricing.manage"
	PermissionAdminMemberships   = "admin.memberships.manage"
	PermissionAdminReleases      = "admin.releases.manage"
	PermissionAdminHelpDocuments = "admin.help.manage"
	PermissionAdminFeedback      = "admin.feedback.manage"
	PermissionAdminAuditRead     = "admin.audit.read"
	PermissionAdminRelay         = "admin.relay.manage"
	PermissionAdminSuper         = "admin.super"
)

var rolePermissions = map[string][]string{
	"user": {
		PermissionAccountRead,
		PermissionAccountWrite,
		PermissionMembershipRedeem,
	},
	"support_admin": {
		PermissionAdminUsersRead,
		PermissionAdminFeedback,
	},
	"content_admin": {
		PermissionAdminPricing,
		PermissionAdminHelpDocuments,
	},
	"membership_admin": {
		PermissionAdminUsersRead,
		PermissionAdminMemberships,
	},
	"release_admin": {
		PermissionAdminReleases,
	},
	"super_admin": {
		PermissionAdminUsersRead,
		PermissionAdminUsersManage,
		PermissionAdminPricing,
		PermissionAdminMemberships,
		PermissionAdminReleases,
		PermissionAdminHelpDocuments,
		PermissionAdminFeedback,
		PermissionAdminAuditRead,
		PermissionAdminRelay,
		PermissionAdminSuper,
	},
}

func PermissionsForRoles(roles []string) []string {
	unique := make(map[string]struct{})
	for _, permission := range rolePermissions["user"] {
		unique[permission] = struct{}{}
	}
	for _, role := range roles {
		for _, permission := range rolePermissions[role] {
			unique[permission] = struct{}{}
		}
	}
	permissions := make([]string, 0, len(unique))
	for permission := range unique {
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return permissions
}

func HasPermission(user User, permission string) bool {
	for _, candidate := range PermissionsForRoles(user.Roles) {
		if candidate == permission {
			return true
		}
	}
	return false
}
