package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

func writeExplicitSourceManifest(t *testing.T) string {
	t.Helper()
	sourceDir := filepath.Join(t.TempDir(), "explicit.test")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := `[package]
id = "explicit.test"
version = "1.0.0"

[[component]]
id = "main"
mode = "hosted"
role = "provider"
entrypoint = "main.wasm"
exports = ["explicit.test.hello"]

[[capability]]
id = "explicit.test.hello"
name = "Hello"
description = "Return a greeting"
schema = """{"type":"object","properties":{"name":{"type":"string"}}}"""

[capability.authorization]
resource_type = "test.resource"

[capability.execution]
effect_target = "none"
replay = "safe"
confirmation_floor = "policy"
`
	if err := os.WriteFile(filepath.Join(sourceDir, "ailuo.toml"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	return sourceDir
}

func TestResolveSourceRequiresExplicitManifest(t *testing.T) {
	if _, _, err := resolveSource(t.Context(), t.TempDir()); err == nil {
		t.Fatal("resolveSource accepted a source directory without ailuo.toml")
	}
}

func TestResolveSDKSourceReadsExplicitContract(t *testing.T) {
	sourceDir := writeExplicitSourceManifest(t)
	packageID, capabilitiesJSON, err := resolveSDKSource(t.Context(), sourceDir)
	if err != nil {
		t.Fatalf("resolveSDKSource: %v", err)
	}
	if packageID != "explicit.test" || len(capabilitiesJSON) == 0 {
		t.Fatalf("package ID/capabilities = %q/%s, want declared capabilities", packageID, capabilitiesJSON)
	}
	var capabilities []map[string]any
	if err := json.Unmarshal(capabilitiesJSON, &capabilities); err != nil || len(capabilities) != 1 || capabilities[0]["id"] != "explicit.test.hello" {
		t.Fatalf("capabilities=%s err=%v", capabilitiesJSON, err)
	}
}

func TestResolveSDKSourceRequiresExplicitManifest(t *testing.T) {
	if _, _, err := resolveSDKSource(t.Context(), t.TempDir()); err == nil {
		t.Fatal("resolveSDKSource accepted a source directory without ailuo.toml")
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
