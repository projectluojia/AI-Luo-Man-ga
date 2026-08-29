package packmgr_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

// writeSourcePackage 在临时目录构造单组件源包（manifest.json + entrypoint 工件）。
func writeSourcePackage(t *testing.T, dir, id, version, mode, artifactName string, deps []packagecontract.Dependency) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("artifact-bytes-" + id + "-" + version)
	if err := os.WriteFile(filepath.Join(dir, artifactName), artifact, 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: id, Version: version,
		Dependencies: deps,
		Components: []packagecontract.Component{{
			ID: "core", Mode: mode, Entrypoint: artifactName,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestInstallReplacesVersionsAndVerifiesIntegrity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourceV1 := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, sourceV1, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)

	record, err := packmgr.Install(ctx, root, sourceV1)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if record.Manifest.ID != "demo.pkg" || record.Manifest.Version != "1.0.0" {
		t.Fatalf("record = %+v, want demo.pkg@1.0.0", record.Manifest)
	}
	// 回读一致。
	reloaded, err := packmgr.ReadInstalled(ctx, filepath.Join(root, "demo.pkg"))
	if err != nil {
		t.Fatalf("ReadInstalled: %v", err)
	}
	if reloaded.Manifest.Version != "1.0.0" {
		t.Fatalf("reloaded version = %s", reloaded.Manifest.Version)
	}
	// 同版本重复安装报错。
	if _, err := packmgr.Install(ctx, root, sourceV1); err == nil {
		t.Fatal("Install same version = nil, want error")
	}
	// 新版本替换。
	sourceV2 := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, sourceV2, "demo.pkg", "2.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	record, err = packmgr.Install(ctx, root, sourceV2)
	if err != nil {
		t.Fatalf("Install v2: %v", err)
	}
	if record.Manifest.Version != "2.0.0" {
		t.Fatalf("record version = %s, want 2.0.0", record.Manifest.Version)
	}
	// 篡改工件后回读失败（完整性校验）。
	artifactPath := filepath.Join(root, "demo.pkg", "app.wasm")
	if err := os.WriteFile(artifactPath, []byte("tampered"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packmgr.ReadInstalled(ctx, filepath.Join(root, "demo.pkg")); err == nil {
		t.Fatal("ReadInstalled after artifact tampering = nil, want error")
	}
}

func TestInstallResolvesDependenciesAgainstInstalled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	depSource := filepath.Join(t.TempDir(), "dep")
	writeSourcePackage(t, depSource, "demo.dep", "1.2.0", packagecontract.ModeHosted, "dep.wasm", nil)
	if _, err := packmgr.Install(ctx, root, depSource); err != nil {
		t.Fatal(err)
	}
	// 缺依赖安装失败。
	appSource := filepath.Join(t.TempDir(), "app")
	writeSourcePackage(t, appSource, "demo.app", "1.0.0", packagecontract.ModeHosted, "app.wasm",
		[]packagecontract.Dependency{{ID: "demo.dep", Constraint: "^2.0.0"}})
	if _, err := packmgr.Install(ctx, root, appSource); err == nil || !strings.Contains(err.Error(), "缺少依赖") {
		t.Fatalf("Install with unsatisfied dependency error = %v, want missing dependency", err)
	}
	// 依赖满足后安装成功。
	writeSourcePackage(t, appSource, "demo.app", "1.0.0", packagecontract.ModeHosted, "app.wasm",
		[]packagecontract.Dependency{{ID: "demo.dep", Constraint: "^1.0.0"}})
	if _, err := packmgr.Install(ctx, root, appSource); err != nil {
		t.Fatalf("Install with satisfied dependency: %v", err)
	}
}

func TestInstallIsolatedWritesProcessSpec(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "isolated")
	writeSourcePackage(t, source, "demo.isolated", "1.0.0", packagecontract.ModeIsolated, "app", nil)
	record, err := packmgr.Install(context.Background(), root, source)
	if err != nil {
		t.Fatalf("Install isolated: %v", err)
	}
	if len(record.Lock.Artifacts) != 1 || record.Lock.Artifacts[0].Process == nil ||
		record.Lock.Artifacts[0].Process.Path != filepath.Join(root, "demo.isolated", "app") {
		t.Fatalf("locked artifact = %+v, want installed process spec", record.Lock.Artifacts)
	}
}

func TestInstallRejectsBreakingReverseDependency(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	depV1 := filepath.Join(t.TempDir(), "dep-v1")
	writeSourcePackage(t, depV1, "demo.dep", "1.0.0", packagecontract.ModeHosted, "dep.wasm", nil)
	if _, err := packmgr.Install(ctx, root, depV1); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(t.TempDir(), "app")
	writeSourcePackage(t, app, "demo.app", "1.0.0", packagecontract.ModeHosted, "app.wasm",
		[]packagecontract.Dependency{{ID: "demo.dep", Constraint: "^1.0.0"}})
	if _, err := packmgr.Install(ctx, root, app); err != nil {
		t.Fatal(err)
	}
	depV2 := filepath.Join(t.TempDir(), "dep-v2")
	writeSourcePackage(t, depV2, "demo.dep", "2.0.0", packagecontract.ModeHosted, "dep.wasm", nil)
	if _, err := packmgr.Install(ctx, root, depV2); err == nil {
		t.Fatal("Install breaking dependency = nil")
	}
	if err := packmgr.Uninstall(ctx, root, "demo.dep"); err == nil {
		t.Fatal("Uninstall depended-on package = nil")
	}
}

func TestInstallRejectsInvalidSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cases := []struct {
		name   string
		mutate func(string)
	}{
		{name: "missing manifest", mutate: func(_ string) {}},
		{name: "invalid manifest json", mutate: func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o640)
		}},
		{name: "missing artifact", mutate: func(dir string) {
			writeSourcePackage(t, dir, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
			os.Remove(filepath.Join(dir, "app.wasm"))
		}},
		{name: "entrypoint escapes source dir", mutate: func(dir string) {
			manifest := []byte(`{"schema_version":"ailuo.package.v2","id":"demo.pkg","version":"1.0.0","components":[{"id":"core","mode":"hosted","entrypoint":"../outside"}]}`)
			os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o640)
		}},
		{name: "entrypoint uses foreign separator", mutate: func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":"ailuo.package.v2","id":"demo.pkg","version":"1.0.0","components":[{"id":"core","mode":"hosted","entrypoint":"..\\outside"}]}`), 0o640)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "src")
			if err := os.MkdirAll(source, 0o750); err != nil {
				t.Fatal(err)
			}
			tc.mutate(source)
			if _, err := packmgr.Install(ctx, root, source); err == nil {
				t.Fatalf("Install(%s) = nil, want error", tc.name)
			}
		})
	}
}

func TestInstallRejectsCorruptExistingPackage(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo.pkg")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "manifest.json"), []byte("broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	if _, err := packmgr.Install(context.Background(), root, source); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("Install corrupt existing package error = %v, want validation failure", err)
	}
}

func TestReadInstalledRejectsArtifactOutsidePackage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	if _, err := packmgr.Install(ctx, root, source); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "app.wasm")
	outsideBody := []byte("outside")
	if err := os.WriteFile(outsidePath, outsideBody, 0o640); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "demo.pkg", "lock.json")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock packagecontract.Lock
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(outsideBody)
	lock.Artifacts[0].Path = outsidePath
	lock.Artifacts[0].SHA256 = hex.EncodeToString(digest[:])
	lockBytes, err = json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packmgr.ReadInstalled(ctx, filepath.Join(root, "demo.pkg")); err == nil {
		t.Fatal("ReadInstalled accepted artifact outside package directory")
	}
}

func TestUpgradeAndUninstall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	if _, err := packmgr.Install(ctx, root, source); err != nil {
		t.Fatal(err)
	}
	// 未安装目标。
	if _, err := packmgr.Upgrade(ctx, root, "missing.pkg", source); err == nil {
		t.Fatal("Upgrade missing package = nil, want error")
	}
	// 同版本。
	if _, err := packmgr.Upgrade(ctx, root, "demo.pkg", source); err == nil {
		t.Fatal("Upgrade same version = nil, want error")
	}
	// 版本变化。
	sourceV2 := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, sourceV2, "demo.pkg", "1.1.0", packagecontract.ModeHosted, "app.wasm", nil)
	record, err := packmgr.Upgrade(ctx, root, "demo.pkg", sourceV2)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if record.Manifest.Version != "1.1.0" {
		t.Fatalf("upgraded version = %s, want 1.1.0", record.Manifest.Version)
	}
	// list 反映当前状态。
	records, err := packmgr.ListInstalled(ctx, root)
	if err != nil || len(records) != 1 || records[0].Manifest.Version != "1.1.0" {
		t.Fatalf("ListInstalled = %+v err=%v", records, err)
	}
	// 卸载。
	if err := packmgr.Uninstall(ctx, root, "demo.pkg"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	records, err = packmgr.ListInstalled(ctx, root)
	if err != nil || len(records) != 0 {
		t.Fatalf("ListInstalled after uninstall = %+v err=%v", records, err)
	}
	// 非包目录不可卸载。
	if err := packmgr.Uninstall(ctx, root, "demo.pkg"); err == nil {
		t.Fatal("Uninstall missing = nil, want error")
	}
}

// 工件按 basename 平铺，两个组件的 entrypoint 同名会互相覆盖并让 lock 的两条
// 记录指向同一个文件，必须在读源阶段拒绝。
func TestInstallRejectsEntrypointBasenameCollision(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "mod.wasm"), []byte("bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.pkg", Version: "1.0.0",
		Components: []packagecontract.Component{
			{ID: "one", Mode: packagecontract.ModeHosted, Entrypoint: "mod.wasm"},
			{ID: "two", Mode: packagecontract.ModeHosted, Entrypoint: "mod.wasm"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err = packmgr.Install(context.Background(), t.TempDir(), source)
	if !errors.Is(err, packagecontract.ErrInvalidFormat) || !strings.Contains(err.Error(), "同名") {
		t.Fatalf("Install error = %v, want basename collision ErrInvalidFormat", err)
	}
}

// isolated 组件必须在 lock 中固化进程规格（可执行文件 = 工件、工作目录 = 包目录、
// 本机 Unix socket），否则宿主装载时拿不到启动参数。
func TestInstallLocksProcessSpecForIsolatedComponent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.svc", "1.0.0", packagecontract.ModeIsolated, "svc-bin", nil)
	record, err := packmgr.Install(ctx, root, source)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(record.Lock.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(record.Lock.Artifacts))
	}
	artifact := record.Lock.Artifacts[0]
	if artifact.Process == nil {
		t.Fatal("isolated 组件缺少进程规格")
	}
	targetDir := filepath.Join(root, "demo.svc")
	if artifact.Process.Path != filepath.Join(targetDir, "svc-bin") ||
		artifact.Process.WorkDir != targetDir {
		t.Fatalf("process spec = %+v, want path/workdir 位于 %s", artifact.Process, targetDir)
	}
	if !packagecontract.IsLocalRuntimeAddress(artifact.Process.Address) {
		t.Fatalf("process address = %q, want 本机地址", artifact.Process.Address)
	}
}

func TestInstallSerializesSamePackagePublication(t *testing.T) {
	root := t.TempDir()
	sources := make([]string, 2)
	for i, version := range []string{"1.0.0", "2.0.0"} {
		sources[i] = filepath.Join(t.TempDir(), "pkg")
		writeSourcePackage(t, sources[i], "demo.concurrent", version, packagecontract.ModeHosted, "app.wasm", nil)
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, len(sources))
	var group sync.WaitGroup
	for _, source := range sources {
		group.Add(1)
		go func(source string) {
			defer group.Done()
			<-start
			_, err := packmgr.Install(context.Background(), root, source)
			errorsSeen <- err
		}(source)
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Install: %v", err)
		}
	}
	record, err := packmgr.ReadInstalled(context.Background(), filepath.Join(root, "demo.concurrent"))
	if err != nil {
		t.Fatalf("ReadInstalled after concurrent Install: %v", err)
	}
	if record.Manifest.Version != "1.0.0" && record.Manifest.Version != "2.0.0" {
		t.Fatalf("version=%s, want one complete installation", record.Manifest.Version)
	}
}

// Install 的阶段目录与备份目录建在安装根内（同文件系统才能原子 rename），崩溃后
// 可能残留；ListInstalled 必须跳过它们，否则一次失败的安装会永久破坏 list 与依赖
// 解析。其他非包条目仍然 fail closed。
func TestListInstalledSkipsInternalWorkDirs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	if _, err := packmgr.Install(ctx, root, source); err != nil {
		t.Fatal(err)
	}
	for _, leftover := range []string{".stage-123456", ".backup-654321"} {
		if err := os.MkdirAll(filepath.Join(root, leftover), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	records, err := packmgr.ListInstalled(ctx, root)
	if err != nil || len(records) != 1 || records[0].Manifest.ID != "demo.pkg" {
		t.Fatalf("ListInstalled = %+v err=%v, want 仅 demo.pkg", records, err)
	}
	// 其他隐藏条目不属于安装器内部目录，仍然报错。
	if err := os.MkdirAll(filepath.Join(root, ".sneaky"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := packmgr.ListInstalled(ctx, root); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("ListInstalled with unknown hidden entry error = %v, want ErrInvalidFormat", err)
	}
}
