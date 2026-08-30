package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/configui"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestLoadConfigUsesControlPlaneDefaults(t *testing.T) {
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	config, err := loadConfig()
	if err != nil || config.configUIAddress != configui.DefaultAddress {
		t.Fatalf("config=%+v error=%v", config, err)
	}
}

func TestLoadConfigRejectsInvalidBoolean(t *testing.T) {
	t.Setenv("AILUO_MANAGE_AGENT", "sometimes")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_MANAGE_AGENT must be a boolean") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsSourcePathLogging(t *testing.T) {
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_LOG_SOURCE", "true")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must be false") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsDemoDataInProduction(t *testing.T) {
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_ENVIRONMENT", "production")
	t.Setenv("AILUO_LOAD_DEMO_DATA", "true")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_LOAD_DEMO_DATA must be false") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsRelativeRuntimeInstallRoot(t *testing.T) {
	t.Setenv("AILUO_MANAGE_AGENT", "false")
	t.Setenv("AILUO_RUNTIME_INSTALL_ROOT", "relative/runtime")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
		t.Fatalf("error=%v", err)
	}
}

func TestDefaultInstallRootIsAbsoluteOrEmpty(t *testing.T) {
	root := packmgr.DefaultInstallRoot()
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) || !strings.HasSuffix(root, "ailuo"+string(filepath.Separator)+"runtime") {
		t.Fatalf("DefaultInstallRoot()=%q 应为绝对路径并结尾为 ailuo/runtime", root)
	}
}

func TestConfigureInstalledRuntimesAllowsEmptySecureCatalog(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	hosts, records, err := configureInstalledRuntimes(t.Context(), config{runtimeInstallRoot: root}, nil)
	if err != nil {
		t.Fatalf("configure empty catalog: %v", err)
	}
	if len(hosts) != 0 || len(records) != 0 {
		t.Fatal("empty catalog must return no hosts/records")
	}
}

func TestConfigureInstalledRuntimesRegistersHostedCatalog(t *testing.T) {
	root := writeInstalledFixture(t)
	if _, _, err := configureInstalledRuntimes(t.Context(), config{runtimeInstallRoot: root}, nil); err == nil || !strings.Contains(err.Error(), "AILUO_RUNTIME_HOST_ADDRESS") {
		t.Fatalf("missing hosted address error=%v", err)
	}
	hosts, records, err := configureInstalledRuntimes(t.Context(), config{
		runtimeInstallRoot: root, runtimeHostAddress: "unix:" + filepath.Join(root, "host.sock"),
	}, nil)
	if err != nil {
		t.Fatalf("configure hosted catalog: %v", err)
	}
	if len(hosts) != 1 || len(records) != 1 {
		t.Fatalf("hosts=%d records=%d", len(hosts), len(records))
	}
	target := registry.New()
	manager, err := loader.New(hosts...)
	if err != nil {
		t.Fatalf("create runtime loader: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown runtime manager: %v", err)
		}
	})
	if err := loader.RegisterInstalled(t.Context(), manager, target, records); err != nil {
		t.Fatalf("register installed runtimes: %v", err)
	}
	if pinned := manager.Pinned(); len(pinned) != 1 || pinned[0] != records[0].Runtime.ID {
		t.Fatalf("pinned=%v", pinned)
	}
	if _, _, err := target.ResolveCapability("main.extension.query"); err != nil {
		t.Fatalf("installed capability not registered: %v", err)
	}
}

func writeInstalledFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "main.extension")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifactBody := []byte("hosted artifact")
	artifact := filepath.Join(directory, "runtime-artifact")
	if err := os.WriteFile(artifact, artifactBody, 0o640); err != nil {
		t.Fatal(err)
	}
	extensions, err := json.Marshal(map[string]any{
		"service": capability.ServiceSpec{ID: "main.extension", Version: "1.0.0", Description: "主程序扩展接线测试"},
		"capabilities": []capability.CapabilitySpec{{
			ID: "main.extension.query", Version: "1.0.0", Name: "扩展查询", Description: "查询测试扩展",
			ServiceID: "main.extension", InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect: capability.SideEffectRead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "main.extension", Version: "1.0.0", Pin: true,
		Extensions: extensions,
		Components: []packagecontract.Component{{ID: "main.extension", Mode: loader.ModeHosted, Entrypoint: "runtime-artifact", Exports: []string{"main.extension.query"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	artifactDigest := sha256.Sum256(artifactBody)
	lockBytes, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: "main.extension", PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts:      []packagecontract.LockedArtifact{{ComponentID: "main.extension", Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	return root
}
