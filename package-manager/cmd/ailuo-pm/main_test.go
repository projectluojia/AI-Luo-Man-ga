package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	packageID, extensions, err := resolveSDKSource(t.Context(), sourceDir)
	if err != nil {
		t.Fatalf("resolveSDKSource: %v", err)
	}
	if packageID != "autogen.test" || len(extensions) == 0 {
		t.Fatalf("package ID/extensions = %q/%s, want extracted extensions", packageID, extensions)
	}
}
