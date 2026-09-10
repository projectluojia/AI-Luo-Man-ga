//go:build windows

package config

import "testing"

func TestPrivateFileACLIsRestrictedBeforePublish(t *testing.T) {
	path := t.TempDir() + `\secret`
	if err := writePrivateFile(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if !fileACLRestrictedToCurrentUser(path) {
		t.Fatal("private file ACL is not restricted to the current user")
	}
	if fileACLRestrictedToCurrentUser(path + ".missing") {
		t.Fatal("missing file was reported as ACL restricted")
	}
}
