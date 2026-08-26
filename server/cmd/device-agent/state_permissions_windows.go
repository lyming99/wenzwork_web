//go:build windows

package main

import (
	"errors"
	"runtime"

	"golang.org/x/sys/windows"
)

func verifyStateFileSecurity(path string) error {
	// Re-apply a protected allowlist on every load. This fails closed when the
	// current process cannot take ownership of its own local state file.
	return secureStateFile(path)
}

func secureStateFile(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return errors.New("open current Windows security token")
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return errors.New("resolve current Windows user SID")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return errors.New("resolve Windows LocalSystem SID")
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return errors.New("resolve Windows administrators SID")
	}

	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(user.User.Sid)
	pinner.Pin(system)
	pinner.Pin(administrators)
	entries := []windows.EXPLICIT_ACCESS{
		stateFileAccess(user.User.Sid, windows.TRUSTEE_IS_USER),
		stateFileAccess(system, windows.TRUSTEE_IS_USER),
		stateFileAccess(administrators, windows.TRUSTEE_IS_GROUP),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return errors.New("build protected Windows state ACL")
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return errors.New("protect Windows agent state ACL")
	}
	return nil
}

func stateFileAccess(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
