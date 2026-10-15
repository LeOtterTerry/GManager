//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestExistingTokenACLTightenedOrRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	body := strings.Repeat("b", MinTokenLength) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := grantEveryoneRead(path); err != nil {
		t.Fatal(err)
	}
	open, err := tokenOpenToOthers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !open {
		t.Fatal("Everyone read ACE was not visible")
	}
	t.Setenv("GMANAGER_TOKEN", "")
	if _, err := ResolveToken(dir); err != nil {
		t.Fatal(err)
	}
	open, err = tokenOpenToOthers(path)
	if err != nil {
		t.Fatal(err)
	}
	if open {
		t.Fatal("token file is still readable by other users")
	}

	if err := grantEveryoneRead(path); err != nil {
		t.Fatal(err)
	}
	orig := restrictTokenFile
	restrictTokenFile = func(string) error { return nil }
	t.Cleanup(func() { restrictTokenFile = orig })
	if _, err := ResolveToken(dir); err == nil {
		t.Fatal("token file stayed readable and startup continued")
	}
	restrictTokenFile = func(string) error { return errors.New("acl") }
	if _, err := ResolveToken(dir); err == nil {
		t.Fatal("ACL update failure was ignored")
	}
}

func grantEveryoneRead(path string) error {
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		return err
	}
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(user),
			},
		},
		{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(everyone),
			},
		},
	}
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
