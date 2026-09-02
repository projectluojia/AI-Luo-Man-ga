package packagefmt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
)

func TestParseProjectUsesExplicitDependencySources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProjectFileName)
	if err := os.WriteFile(path, []byte(`
[project]
id = "ailuo"

[dependencies."demo.local"]
version = "^1.0.0"
path = "packages/demo"

[dependencies."demo.remote"]
version = ">=2.0.0,<3.0.0"
registry = "github:owner/demo"
`), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseProject(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != projectcontract.SchemaVersion || len(manifest.Dependencies) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Dependencies[0].ID != "demo.local" || manifest.Dependencies[0].Source != "path:packages/demo" ||
		manifest.Dependencies[1].ID != "demo.remote" || manifest.Dependencies[1].Source != "github:owner/demo" {
		t.Fatalf("dependencies = %+v", manifest.Dependencies)
	}
}

func TestParseProjectRejectsAmbiguousOrUnknownSource(t *testing.T) {
	for _, body := range []string{
		`[project]
id = "ailuo"
[dependencies."demo.pkg"]
version = "1.0.0"
`,
		`[project]
id = "ailuo"
[dependencies."demo.pkg"]
version = "1.0.0"
path = "packages/demo"
registry = "github:owner/demo"
`,
		`[project]
id = "ailuo"
[dependencies."demo.pkg"]
version = "1.0.0"
path = "../demo"
`,
	} {
		path := filepath.Join(t.TempDir(), ProjectFileName)
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseProject(path); !errors.Is(err, ErrSourceInvalid) {
			t.Fatalf("ParseProject(%q) = %v, want ErrSourceInvalid", body, err)
		}
	}
}

func TestParseProjectPathUsesPackagePathRules(t *testing.T) {
	if err := packagecontract.ValidateSource("path:packages/demo"); err != nil {
		t.Fatal(err)
	}
}

func TestParseProjectRejectsSymlinkEscapingProject(t *testing.T) {
	projectDir := t.TempDir()
	outsideDir := t.TempDir()
	link := filepath.Join(projectDir, "linked")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("当前平台不允许创建目录符号链接: %v", err)
	}
	path := filepath.Join(projectDir, ProjectFileName)
	if err := os.WriteFile(path, []byte(`
[project]
id = "ailuo"

[dependencies."demo.pkg"]
version = "1.0.0"
path = "linked"
`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProject(path); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("ParseProject(symlink escape) = %v, want ErrSourceInvalid", err)
	}
}

func TestResolveLocalDependencyPathAcceptsSiblingWithinProject(t *testing.T) {
	projectDir := t.TempDir()
	baseDir := filepath.Join(projectDir, "packages", "app")
	targetDir := filepath.Join(projectDir, "packages", "dep")
	if err := os.MkdirAll(baseDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(baseDir, "linked")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Skipf("当前平台不允许创建目录符号链接: %v", err)
	}
	resolved, err := ResolveLocalDependencyPath(projectDir, baseDir, "linked")
	if err != nil {
		t.Fatalf("ResolveLocalDependencyPath(in-project sibling symlink) = %v", err)
	}
	expected, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != expected {
		t.Fatalf("resolved = %q, want %q", resolved, expected)
	}
}
