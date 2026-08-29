package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/configui"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
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

func TestMigrateEnabledCapabilitiesRenamesChildControl(t *testing.T) {
	got := migrateEnabledCapabilities([]string{"agent.run", "agent.status", "agent.run", "campus.bus.routes.list"})
	want := map[string]bool{
		kernelecho.CreateChildRunCapabilityID: true,
		kernelecho.GetChildStatusCapabilityID: true,
		campus.BusRouteListCapabilityID:       true,
		promptservice.PreferenceGetID:         true,
		promptservice.PreferenceSetID:         true,
		promptservice.PreferenceResetID:       true,
	}
	if len(got) != len(want) {
		t.Fatalf("capabilities=%v, want %d unique entries", got, len(want))
	}
	for _, capabilityID := range got {
		if !want[capabilityID] {
			t.Fatalf("unexpected capability %q in %v", capabilityID, got)
		}
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

func TestDefaultRuntimeRootIsAbsoluteOrEmpty(t *testing.T) {
	root := defaultRuntimeRoot()
	if root == "" {
		return // 无 HOME / 不支持 UserConfigDir 的平台
	}
	if !filepath.IsAbs(root) || !strings.HasSuffix(root, "ailuo"+string(filepath.Separator)+"runtime") {
		t.Fatalf("defaultRuntimeRoot()=%q 应为绝对路径并结尾为 ailuo/runtime", root)
	}
}

func TestResolveSourceRequiresVersionForZeroDeclarationPackage(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "hello.pkg")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc hello(args HelloArgs) {}\ntype HelloArgs struct { Name string `json:\"name\"` }\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveSource(t.Context(), sourceDir, ""); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("resolveSource error = %v, want required version", err)
	}
}

func TestResolveSDKSourceExtractsZeroDeclarationContract(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "hello.pkg")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc hello(args HelloArgs) {}\ntype HelloArgs struct { Name string `json:\"name\"` }\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	packageID, extensions, err := resolveSDKSource(t.Context(), sourceDir)
	if err != nil {
		t.Fatalf("resolveSDKSource: %v", err)
	}
	if packageID != "hello.pkg" || len(extensions) == 0 {
		t.Fatalf("package ID/extensions = %q/%s, want extracted extensions", packageID, extensions)
	}
}

func TestConfigureInstalledRuntimesAllowsEmptySecureCatalog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("非 Unix 平台显式关闭安装目录属主校验")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	hosts, records, _, err := configureInstalledRuntimes(t.Context(), config{runtimeInstallRoot: root}, nil)
	if err != nil {
		t.Fatalf("configure empty catalog: %v", err)
	}
	if len(hosts) != 0 || len(records) != 0 {
		t.Fatal("empty catalog must return no hosts/records")
	}
}

func TestConfigureInstalledRuntimesRegistersHostedCatalogAndRequiresAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("非 Unix 平台显式关闭安装目录属主校验")
	}
	root := writeMainInstalledFixture(t)
	if _, _, _, err := configureInstalledRuntimes(t.Context(), config{
		runtimeInstallRoot: root,
	}, nil); err == nil || !strings.Contains(err.Error(), "AILUO_RUNTIME_HOST_ADDRESS") {
		t.Fatalf("missing hosted address error=%v", err)
	}

	hosts, records, _, err := configureInstalledRuntimes(t.Context(), config{
		runtimeInstallRoot: root,
		runtimeHostAddress: "unix:" + filepath.Join(root, "host.sock"),
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
	// pin 运行时由各清单声明推导（Pinned()），装配不再单独返回。
	pinned := manager.Pinned()
	if len(pinned) != 1 || pinned[0] != records[0].Runtime.ID {
		t.Fatalf("pinned=%v", pinned)
	}
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
	extensions, err := json.Marshal(map[string]any{
		"service": capability.ServiceSpec{
			ID: "main.extension", Version: "1.0.0", Description: "主程序扩展接线测试",
		},
		"capabilities": []capability.CapabilitySpec{{
			ID: "main.extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询测试扩展", ServiceID: "main.extension",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "main.extension", Version: "1.0.0",
		Pin: true, Extensions: extensions,
		Components: []packmgr.Component{{
			ID: "main.extension", Mode: loader.ModeHosted, Entrypoint: "runtime-artifact",
			Exports: []string{"main.extension.query"},
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
	lockBytes, err := json.Marshal(packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion,
		PackageID:     "main.extension", PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packmgr.LockedArtifact{{
			ComponentID: "main.extension", Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
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

func TestMaintenanceIdentityBindIsIdempotent(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	arguments := []string{
		"identity-bind",
		"--database", database,
		"--user", "user-qq-1",
		"--app", "campus-services",
		"--platform", "qq",
		"--space", "space-qq-1",
		"--platform-user", "openid-qq-1",
	}
	output := &bytes.Buffer{}
	handled, err := runMaintenanceCommand(arguments, output)
	if err != nil || !handled || !strings.Contains(output.String(), "身份开通完成") {
		t.Fatalf("first provision handled=%t output=%q err=%v", handled, output.String(), err)
	}
	// 幂等重放：同一命令重复执行必须成功且不改变结果。
	output.Reset()
	handled, err = runMaintenanceCommand(arguments, output)
	if err != nil || !handled {
		t.Fatalf("replayed provision handled=%t err=%v", handled, err)
	}
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolved, err := identity.NewService(store).ResolveIdentity(t.Context(), "campus-services", "qq", "space-qq-1", "openid-qq-1")
	if err != nil {
		t.Fatalf("resolve bound identity: %v", err)
	}
	if resolved.UserID != "user-qq-1" {
		t.Fatalf("bound user=%q want user-qq-1", resolved.UserID)
	}
}

func TestMaintenanceIdentityBindRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"identity-bind"}, // 缺必填参数
		{"identity-bind", "--database", "relative.db", "--user", "user-1"},              // 相对路径
		{"identity-bind", "--database", "x.db", "--user", "user-1", "--platform", "qq"}, // 绑定参数不全
	}
	for _, arguments := range cases {
		handled, err := runMaintenanceCommand(arguments, &bytes.Buffer{})
		if !handled || err == nil {
			t.Fatalf("arguments=%v handled=%t err=%v", arguments, handled, err)
		}
	}
}

func TestMaintenanceIdentityUnbind(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	bind := []string{"identity-bind", "--database", database, "--user", "user-qq-1", "--platform", "qq", "--space", "private", "--platform-user", "openid-qq-1"}
	if handled, err := runMaintenanceCommand(bind, &bytes.Buffer{}); err != nil || !handled {
		t.Fatalf("bind handled=%t err=%v", handled, err)
	}
	unbind := []string{"identity-unbind", "--database", database, "--platform", "qq", "--space", "private", "--platform-user", "openid-qq-1"}
	output := &bytes.Buffer{}
	if handled, err := runMaintenanceCommand(unbind, output); err != nil || !handled || !strings.Contains(output.String(), "身份解绑完成") {
		t.Fatalf("unbind handled=%t output=%q err=%v", handled, output.String(), err)
	}
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := identity.NewService(store).ResolveIdentity(t.Context(), "campus-services", "qq", "private", "openid-qq-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("resolve after unbind error=%v, want ErrNotFound", err)
	}
	// 再次解绑：身份不存在返回 ErrNotFound，命令仍明确报错。
	handled, err := runMaintenanceCommand(unbind, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("second unbind handled=%t err=%v", handled, err)
	}
}

func TestMaintenanceIdentityBindRejectsConflictingBinding(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	base := []string{"identity-bind", "--database", database, "--platform", "qq", "--space", "space-qq-1", "--platform-user", "openid-qq-1"}
	if handled, err := runMaintenanceCommand(append(append([]string{}, base...), "--user", "user-qq-1"), &bytes.Buffer{}); err != nil || !handled {
		t.Fatalf("first bind handled=%t err=%v", handled, err)
	}
	// 同一外部身份绑定到另一个内部用户必须被拒绝。
	handled, err := runMaintenanceCommand(append(append([]string{}, base...), "--user", "user-qq-2"), &bytes.Buffer{})
	if !handled || err == nil || !strings.Contains(err.Error(), "身份开通失败") {
		t.Fatalf("conflicting bind handled=%t err=%v", handled, err)
	}
}

func TestRuntimeHostCommandRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"runtime-host"}, // 缺必填参数
		{"runtime-host", "--install-root", "relative", "--address", "127.0.0.1:0"},   // 相对安装目录
		{"runtime-host", "--install-root", t.TempDir(), "--address", "0.0.0.0:7000"}, // 非 loopback 监听
	}
	for _, arguments := range cases {
		handled, err := runMaintenanceCommand(arguments, &bytes.Buffer{})
		if !handled || err == nil {
			t.Fatalf("arguments=%v handled=%t err=%v", arguments, handled, err)
		}
	}
}
