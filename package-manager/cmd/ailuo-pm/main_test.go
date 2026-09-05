package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

const zeroDeclarationSource = "package main\nfunc hello(args HelloArgs) {}\ntype HelloArgs struct { Name string `json:\"name\"` }\n"

func writeZeroDeclarationSource(t *testing.T) string {
	t.Helper()
	sourceDir := filepath.Join(t.TempDir(), "autogen.test")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(zeroDeclarationSource), 0o640); err != nil {
		t.Fatal(err)
	}
	return sourceDir
}

func TestResolveSourceRequiresVersionForZeroDeclarationPackage(t *testing.T) {
	sourceDir := writeZeroDeclarationSource(t)
	if _, _, err := resolveSource(t.Context(), sourceDir, ""); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("resolveSource error = %v, want required version", err)
	}
}

func TestResolveSDKSourceExtractsZeroDeclarationContract(t *testing.T) {
	sourceDir := writeZeroDeclarationSource(t)
	packageID, capabilitiesJSON, err := resolveSDKSource(t.Context(), sourceDir)
	if err != nil {
		t.Fatalf("resolveSDKSource: %v", err)
	}
	if packageID != "autogen.test" || len(capabilitiesJSON) == 0 {
		t.Fatalf("package ID/capabilities = %q/%s, want extracted capabilities", packageID, capabilitiesJSON)
	}
}

func TestUnlockRequiresForceAndRemovesInstallLock(t *testing.T) {
	root := t.TempDir()
	lockPath := packageio.InstallRootLockPath(root)
	if err := os.WriteFile(lockPath, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPackageCommand(context.Background(), []string{"unlock", "--root", root}, new(bytes.Buffer)); err == nil {
		t.Fatal("unlock without --force = nil, want configuration error")
	}
	var output bytes.Buffer
	if err := runPackageCommand(context.Background(), []string{"unlock", "--force", "--root", root}, &output); err != nil {
		t.Fatalf("unlock --force: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock after unlock err=%v, want not exist", err)
	}
}
