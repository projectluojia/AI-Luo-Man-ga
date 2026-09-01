package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/configui"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestLoadConfigUsesControlPlaneDefaults(t *testing.T) {
	t.Setenv("AILUO_MANAGE_EXECUTOR", "false")
	config, err := loadConfig()
	if err != nil || config.configUIAddress != configui.DefaultAddress {
		t.Fatalf("config=%+v error=%v", config, err)
	}
}

func TestLoadConfigRejectsInvalidBoolean(t *testing.T) {
	t.Setenv("AILUO_MANAGE_EXECUTOR", "sometimes")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AILUO_MANAGE_EXECUTOR must be a boolean") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsSourcePathLogging(t *testing.T) {
	t.Setenv("AILUO_MANAGE_EXECUTOR", "false")
	t.Setenv("AILUO_LOG_SOURCE", "true")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must be false") {
		t.Fatalf("error=%v", err)
	}
}

func TestLoadConfigRejectsRelativeRuntimeInstallRoot(t *testing.T) {
	t.Setenv("AILUO_MANAGE_EXECUTOR", "false")
	t.Setenv("AILUO_RUNTIME_INSTALL_ROOT", "relative/runtime")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
		t.Fatalf("error=%v", err)
	}
}

func TestInitialCapabilityIDsUseInstalledMetadata(t *testing.T) {
	ids := initialCapabilityIDs(registry.New(), []loader.InstalledRecord{{Capabilities: []capability.CapabilitySpec{
		{ID: "z.capability"}, {ID: "a.capability"}, {ID: "z.capability"},
	}}})
	want := []string{"a.capability", "z.capability"}
	if !slices.Equal(ids, want) {
		t.Fatalf("initial capabilities=%v, want %v", ids, want)
	}
}

func TestDefaultInstallRootIsAbsoluteOrEmpty(t *testing.T) {
	root := defaultRuntimeInstallRoot()
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
	projectRoot := t.TempDir()
	writeEmptyProject(t, projectRoot)
	hosts, records, err := configureInstalledRuntimes(t.Context(), config{projectRoot: projectRoot, runtimeInstallRoot: root}, nil)
	if err != nil {
		t.Fatalf("configure empty catalog: %v", err)
	}
	if len(hosts) != 0 || len(records) != 0 {
		t.Fatal("empty catalog must return no hosts/records")
	}
}

func TestConfigureInstalledRuntimesRegistersHostedCatalog(t *testing.T) {
	root := writeInstalledFixture(t)
	projectRoot := t.TempDir()
	writeProjectLock(t, projectRoot, root)
	if _, _, err := configureInstalledRuntimes(t.Context(), config{projectRoot: projectRoot, runtimeInstallRoot: root}, nil); err == nil || !strings.Contains(err.Error(), "AILUO_RUNTIME_HOST_ADDRESS") {
		t.Fatalf("missing hosted address error=%v", err)
	}
	hosts, records, err := configureInstalledRuntimes(t.Context(), config{
		projectRoot: projectRoot, runtimeInstallRoot: root, runtimeHostAddress: "unix:" + filepath.Join(root, "host.sock"),
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

func TestConfigureInstalledRuntimesIgnoresUnlistedInstalledPackages(t *testing.T) {
	root := writeInstalledFixture(t)
	writeInstalledPackage(t, root, "extra.extension")
	projectRoot := t.TempDir()
	writeProjectLock(t, projectRoot, root)
	hosts, records, err := configureInstalledRuntimes(t.Context(), config{
		projectRoot: projectRoot, runtimeInstallRoot: root,
		runtimeHostAddress: "unix:" + filepath.Join(root, "host.sock"),
	}, nil)
	if err != nil {
		t.Fatalf("configure locked catalog: %v", err)
	}
	if len(hosts) != 1 || len(records) != 1 || records[0].PackageID != "main.extension" {
		t.Fatalf("hosts=%d records=%+v, want only locked package", len(hosts), records)
	}
}

func writeEmptyProject(t *testing.T, projectRoot string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(projectRoot, "ailuo.toml"), []byte("[project]\nid = \"ailuo\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	writeProjectLock(t, projectRoot, "")
}

func writeProjectLock(t *testing.T, projectRoot, installRoot string) {
	t.Helper()
	manifestPath := filepath.Join(projectRoot, "ailuo.toml")
	if _, err := os.Stat(manifestPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(manifestPath, []byte("[project]\nid = \"ailuo\"\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	manifestSHA, err := packageio.HashFile(t.Context(), manifestPath, packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lock := projectcontract.Lock{
		SchemaVersion: projectcontract.SchemaVersion, ProjectID: "ailuo", ProjectManifestSHA256: manifestSHA,
	}
	if installRoot != "" {
		directory := filepath.Join(installRoot, "main.extension")
		packageManifestSHA, err := packageio.HashFile(t.Context(), filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		lockBytes, err := os.ReadFile(filepath.Join(directory, "lock.json"))
		if err != nil {
			t.Fatal(err)
		}
		var packageLock packagecontract.Lock
		if err := packagecontract.DecodeStrictJSON(lockBytes, &packageLock); err != nil {
			t.Fatal(err)
		}
		packageLockSHA, err := packageio.CanonicalLockDigest(t.Context(), directory, packageLock)
		if err != nil {
			t.Fatal(err)
		}
		lock.Packages = []projectcontract.LockedPackage{{
			ID: "main.extension", Version: "1.0.0", Source: "github:owner/main-extension",
			ManifestSHA256: packageManifestSHA, LockSHA256: packageLockSHA,
		}}
	}
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "ailuo.lock"), encoded, 0o640); err != nil {
		t.Fatal(err)
	}
}

func writeInstalledFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeInstalledPackage(t, root, "main.extension")
	return root
}

func writeInstalledPackage(t *testing.T, root, packageID string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, packageID)
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
}
