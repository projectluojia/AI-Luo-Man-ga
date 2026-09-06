package projectmgr_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/projectmgr"
)

func TestSyncResolvesAndLocksTransitiveLocalDependencies(t *testing.T) {
	projectDir := t.TempDir()
	appDir := filepath.Join(projectDir, "packages", "app")
	depDir := filepath.Join(appDir, "dep")
	writePackage(t, depDir, "demo.dep", "1.2.0", "dep.wasm", "")
	writePackage(t, appDir, "demo.app", "1.0.0", "app.wasm", `
[dependencies."demo.dep"]
version = "^1.0.0"
source = "path:dep"
`)
	writeProject(t, projectDir, `
[project]
id = "ailuo"

[dependencies."demo.app"]
version = "^1.0.0"
path = "packages/app"
`)

	root := filepath.Join(projectDir, "runtime")
	lock, err := projectmgr.Sync(context.Background(), filepath.Join(projectDir, "ailuo.toml"), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lock.ProjectID != "ailuo" || len(lock.Packages) != 2 {
		t.Fatalf("lock = %+v", lock)
	}
	if lock.Packages[0].ID != "demo.app" || lock.Packages[1].ID != "demo.dep" {
		t.Fatalf("lock packages = %+v, want stable ID order", lock.Packages)
	}
	for _, id := range []string{"demo.app", "demo.dep"} {
		if _, err := packageio.ReadInstalled(context.Background(), filepath.Join(root, id)); err != nil {
			t.Fatalf("ReadInstalled(%s): %v", id, err)
		}
	}
	lockBytes, err := os.ReadFile(filepath.Join(projectDir, "ailuo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded projectcontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := projectcontract.ValidateLockShape(decoded); err != nil {
		t.Fatal(err)
	}
	// 同一版本、同一清单再次同步应是幂等操作。
	if _, err := projectmgr.Sync(context.Background(), filepath.Join(projectDir, "ailuo.toml"), root, nil); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
}

func TestSyncRejectsSameVersionArtifactDrift(t *testing.T) {
	projectDir := t.TempDir()
	packageDir := filepath.Join(projectDir, "packages", "demo")
	writePackage(t, packageDir, "demo.pkg", "1.0.0", "demo.wasm", "")
	writeProject(t, projectDir, `
[project]
id = "ailuo"

[dependencies."demo.pkg"]
version = "1.0.0"
path = "packages/demo"
`)
	installRoot := filepath.Join(projectDir, "runtime")
	if _, err := projectmgr.Sync(t.Context(), filepath.Join(projectDir, "ailuo.toml"), installRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "demo.wasm"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := projectmgr.Sync(t.Context(), filepath.Join(projectDir, "ailuo.toml"), installRoot, nil); err == nil || !strings.Contains(err.Error(), "内容不一致") {
		t.Fatalf("Sync after same-version artifact drift = %v, want immutable-content error", err)
	}
}

func TestSyncRejectsUnsatisfiedTransitiveConstraint(t *testing.T) {
	projectDir := t.TempDir()
	appDir := filepath.Join(projectDir, "packages", "app")
	writePackage(t, filepath.Join(appDir, "dep"), "demo.dep", "2.0.0", "dep.wasm", "")
	writePackage(t, appDir, "demo.app", "1.0.0", "app.wasm", `
[dependencies."demo.dep"]
version = "^1.0.0"
source = "path:dep"
`)
	writeProject(t, projectDir, `
[project]
id = "ailuo"
[dependencies."demo.app"]
version = "1.0.0"
path = "packages/app"
`)
	_, err := projectmgr.Sync(context.Background(), filepath.Join(projectDir, "ailuo.toml"), filepath.Join(projectDir, "runtime"), nil)
	if err == nil || !strings.Contains(err.Error(), "不满足约束") {
		t.Fatalf("Sync error = %v, want constraint failure", err)
	}
}

func writeProject(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "ailuo.toml"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func writePackage(t *testing.T, dir, id, version, artifact, dependency string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte(id+"-"+version), 0o640); err != nil {
		t.Fatal(err)
	}
	body := "[package]\nid = \"" + id + "\"\nversion = \"" + version + "\"\n\n[[component]]\nid = \"main\"\nmode = \"hosted\"\nentrypoint = \"" + artifact + "\"\n" + dependency
	if err := os.WriteFile(filepath.Join(dir, "ailuo.toml"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}
