//go:build windows

package packageio_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"golang.org/x/sys/windows"
)

func secureTestDirectory(path string) error {
	userSID, err := currentTestUserSIDValue()
	if err != nil {
		return err
	}
	return setTestDACL(path, fmt.Sprintf(
		"(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)",
		userSID,
	))
}

func setTestDACL(path, entries string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P" + entries)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, dacl, nil,
	)
}

func currentTestUserSID(t *testing.T) string {
	t.Helper()
	sid, err := currentTestUserSIDValue()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func currentTestUserSIDValue() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func TestValidateSecureTreeRejectsUntrustedWriteACE(t *testing.T) {
	root := newSecureTestDir(t)
	entries := fmt.Sprintf("(A;;GA;;;%s)(A;;GW;;;BU)", currentTestUserSID(t))
	if err := setTestDACL(root, entries); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecureTree(t.Context(), root); !errors.Is(err, packageio.ErrInsecurePath) {
		t.Fatalf("ValidateSecureTree(untrusted write) = %v, want ErrInsecurePath", err)
	}
}

func TestValidateSecureTreeAllowsUntrustedDenyACE(t *testing.T) {
	root := newSecureTestDir(t)
	userSID := currentTestUserSID(t)
	untrustedSID := "S-1-5-21-1111111111-2222222222-3333333333-4444444444"
	entries := fmt.Sprintf("(D;OICI;GW;;;%s)(A;OICI;GA;;;%s)", untrustedSID, userSID)
	if err := setTestDACL(root, entries); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecureTree(t.Context(), root); err != nil {
		t.Fatalf("ValidateSecureTree(untrusted deny) = %v", err)
	}
}

func TestValidateSecureTreeRejectsInheritedUntrustedWriteACE(t *testing.T) {
	root := newSecureTestDir(t)
	entries := fmt.Sprintf("(A;OICI;GA;;;%s)(A;OICI;GW;;;BU)", currentTestUserSID(t))
	if err := setTestDACL(root, entries); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "artifact")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecurePath(artifact); !errors.Is(err, packageio.ErrInsecurePath) {
		t.Fatalf("ValidateSecurePath(inherited untrusted write) = %v, want ErrInsecurePath", err)
	}
}

func TestValidateSecureTreeRejectsUntrustedDeleteAndACLChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		right string
	}{
		{name: "delete", right: "SD"},
		{name: "write dacl", right: "WD"},
		{name: "write owner", right: "WO"},
		{name: "delete child", right: "DC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newSecureTestDir(t)
			entries := fmt.Sprintf("(A;;GA;;;%s)(A;;%s;;;BU)", currentTestUserSID(t), tc.right)
			if err := setTestDACL(root, entries); err != nil {
				t.Fatal(err)
			}
			if err := packageio.ValidateSecureDirectory(root); !errors.Is(err, packageio.ErrInsecurePath) {
				t.Fatalf("ValidateSecureDirectory(%s) = %v, want ErrInsecurePath", tc.right, err)
			}
		})
	}
}

func TestValidateSecureTreeAllowsTrustedWriters(t *testing.T) {
	userSID := currentTestUserSID(t)
	for _, tc := range []struct {
		name string
		sid  string
	}{
		{name: "current user", sid: userSID},
		{name: "local system", sid: "SY"},
		{name: "administrators", sid: "BA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newSecureTestDir(t)
			entries := fmt.Sprintf("(A;OICI;GA;;;%s)(A;OICI;GW;;;%s)", userSID, tc.sid)
			if err := setTestDACL(root, entries); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("artifact"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := packageio.ValidateSecureTree(t.Context(), root); err != nil {
				t.Fatalf("ValidateSecureTree(%s) = %v", tc.name, err)
			}
		})
	}
}

func TestValidateSecureTreeRechecksChangedACL(t *testing.T) {
	root := newSecureTestDir(t)
	artifact := filepath.Join(root, "artifact")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecureTree(t.Context(), root); err != nil {
		t.Fatalf("initial ValidateSecureTree = %v", err)
	}
	entries := fmt.Sprintf("(A;;GA;;;%s)(A;;GW;;;BU)", currentTestUserSID(t))
	if err := setTestDACL(artifact, entries); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecureTree(t.Context(), root); !errors.Is(err, packageio.ErrInsecurePath) {
		t.Fatalf("ValidateSecureTree(after ACL change) = %v, want ErrInsecurePath", err)
	}
}
