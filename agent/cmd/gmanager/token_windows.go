//go:build windows

package main

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

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

// tokenOpenToOthers reports whether any allow ACE grants read to a SID other
// than the current user. A missing or null DACL grants access to everyone.
func tokenOpenToOthers(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return true, nil
		}
		return false, err
	}
	if dacl == nil {
		return true, nil
	}
	user, err := currentUserSID()
	if err != nil {
		return false, err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
				return true, nil
			}
			continue
		}
		mask := uint32(ace.Mask)
		if mask&windows.FILE_READ_DATA == 0 && mask&windows.GENERIC_READ == 0 && mask&windows.GENERIC_ALL == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid == nil || !sid.Equals(user) {
			return true, nil
		}
	}
	return false, nil
}

func currentUserSID() (sid *windows.SID, err error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}
