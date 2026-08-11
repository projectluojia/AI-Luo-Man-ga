package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestLoadConfigRequiresModel(t *testing.T) {
	t.Setenv("AILUO_MODEL", "")
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_MODEL is required") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsInvalidBoolean(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "sometimes")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_MANAGE_AGENT must be a boolean") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsSourcePathLogging(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_LOG_SOURCE", "true")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must be false") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRequiresModelKeyForManagedAgent(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "true")
	t.Setenv("AILUO_MODEL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AILUO_MODEL_API_KEY_FILE", "")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "MODEL_API_KEY") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRequiresRestrictedSecretFileInProduction(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "true")
	t.Setenv("AILUO_ENVIRONMENT", "production")
	t.Setenv("AILUO_MODEL_API_KEY", "raw-secret")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AILUO_MODEL_API_KEY_FILE", "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must use AILUO_MODEL_API_KEY_FILE") {
		t.Fatalf("raw production secret error=%v", err)
	}

	secretPath := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(secretPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AILUO_MODEL_API_KEY", "")
	t.Setenv("AILUO_MODEL_API_KEY_FILE", secretPath)
	if !unixSecurityAvailable {
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "owner-only permission verification") {
			t.Fatalf("unsupported secret file verification error=%v", err)
		}
		return
	}
	if _, err := loadConfig(); err != nil {
		t.Fatalf("restricted secret file rejected: %v", err)
	}
	if err := os.Chmod(secretPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "no group or other permissions") {
		t.Fatalf("insecure secret file error=%v", err)
	}
	if err := os.Chmod(secretPath, 0o600); err != nil {
		t.Fatal(err)
	}
	secretLink := filepath.Join(t.TempDir(), "model-key-link")
	if err := os.Symlink(secretPath, secretLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AILUO_MODEL_API_KEY_FILE", secretLink)
	if _, err := loadConfig(); err == nil {
		t.Fatal("symlinked production secret was accepted")
	}
}

func TestLoadConfigRejectsDemoDataInProduction(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_ENVIRONMENT", "production")
	t.Setenv("AILUO_LOAD_DEMO_DATA", "true")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_LOAD_DEMO_DATA must be false") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsRelativeRuntimeInstallRoot(t *testing.T) {
	t.Setenv("AILUO_MODEL", "test")
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_RUNTIME_INSTALL_ROOT", "relative/runtime")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigureInstalledRuntimesAllowsEmptySecureCatalog(t *testing.T) {
	if !unixSecurityAvailable {
		t.Skip("非 Unix 平台显式关闭安装目录属主校验")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := configureInstalledRuntimes(t.Context(), config{runtimeInstallRoot: root}, registry.New())
	if err != nil {
		t.Fatalf("configure empty catalog: %v", err)
	}
	if manager != nil {
		t.Fatal("empty catalog created a runtime manager")
	}
}

func TestConfigureInstalledRuntimesRegistersHostedCatalogAndRequiresAddress(t *testing.T) {
	if !unixSecurityAvailable {
		t.Skip("非 Unix 平台显式关闭安装目录属主校验")
	}
	root := writeMainInstalledFixture(t)
	if _, err := configureInstalledRuntimes(t.Context(), config{
		runtimeInstallRoot: root,
	}, registry.New()); err == nil || !strings.Contains(err.Error(), "AILUO_RUNTIME_HOST_ADDRESS") {
		t.Fatalf("missing hosted address error=%v", err)
	}

	target := registry.New()
	manager, err := configureInstalledRuntimes(t.Context(), config{
		runtimeInstallRoot: root,
		runtimeHostAddress: "unix:" + filepath.Join(root, "host.sock"),
	}, target)
	if err != nil {
		t.Fatalf("configure hosted catalog: %v", err)
	}
	if manager == nil {
		t.Fatal("hosted catalog did not create a runtime manager")
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(t.Context()); err != nil {
			t.Errorf("shutdown runtime manager: %v", err)
		}
	})
	if _, _, err := target.ResolveCapability("main.extension.query"); err != nil {
		t.Fatalf("installed capability not registered: %v", err)
	}
}

func writeMainInstalledFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "main.extension")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(directory, "runtime-artifact")
	artifactBody := []byte("hosted artifact")
	if err := os.WriteFile(artifact, artifactBody, 0o640); err != nil {
		t.Fatal(err)
	}
	installed := loader.InstalledManifest{
		SchemaVersion: loader.InstallSchemaVersion,
		Runtime: loader.InstalledRuntimeSpec{
			ID: "main.extension", Version: "1.0.0", Mode: loader.ModeHosted,
		},
		Service: registry.ServiceSpec{
			ID: "main.extension", Version: "1.0.0", Description: "主程序扩展接线测试",
		},
		Capabilities: []registry.CapabilitySpec{{
			ID: "main.extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询测试扩展", ServiceID: "main.extension",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      registry.SideEffectRead,
		}},
	}
	manifest, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	artifactDigest := sha256.Sum256(artifactBody)
	lockBytes, err := json.Marshal(loader.InstalledLock{
		SchemaVersion: loader.InstallSchemaVersion,
		RuntimeID:     "main.extension", RuntimeVersion: "1.0.0", Mode: loader.ModeHosted,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		ArtifactSHA256: hex.EncodeToString(artifactDigest[:]),
		ArtifactPath:   artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMaintenanceBackupValidateAndRestoreCommands(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "source.db")
	backup := filepath.Join(directory, "backup.db")
	restored := filepath.Join(directory, "restored.db")
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	handled, err := runMaintenanceCommand([]string{"backup", "--database", database, "--destination", backup}, output)
	if err != nil || !handled || !strings.Contains(output.String(), "完整性校验") {
		t.Fatalf("backup handled=%t output=%q err=%v", handled, output.String(), err)
	}
	output.Reset()
	handled, err = runMaintenanceCommand([]string{"validate-backup", "--backup", backup}, output)
	if err != nil || !handled {
		t.Fatalf("validate handled=%t err=%v", handled, err)
	}
	output.Reset()
	handled, err = runMaintenanceCommand([]string{"restore", "--backup", backup, "--destination", restored}, output)
	if err != nil || !handled {
		t.Fatalf("restore handled=%t err=%v", handled, err)
	}
	opened, err := sqlite.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
}

func TestMaintenanceCommandsRejectAmbiguousOrDestructiveTargets(t *testing.T) {
	handled, err := runMaintenanceCommand([]string{"backup", "--database", "relative.db", "--destination", "backup.db"}, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("relative paths handled=%t err=%v", handled, err)
	}
	handled, err = runMaintenanceCommand([]string{"unknown"}, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("unknown command handled=%t err=%v", handled, err)
	}
}
