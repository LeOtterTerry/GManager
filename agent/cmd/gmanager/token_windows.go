//go:build windows

package main

import "golang.org/x/sys/windows"

// restrictToOwner replaces the DACL with a protected ACE granting the current
// user full control. Inherited permissions are not kept.
func restrictToOwner(path string) (err error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}
