package packageio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

func TestHashArtifactExcludesDevelopmentCaches(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(root, "runtime.py"), []byte("print('ok')"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "__pycache__"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "__pycache__", "runtime.pyc"), []byte(root), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	firstDigest, err := packageio.HashArtifact(context.Background(), first, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := packageio.HashArtifact(context.Background(), second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("cache contents changed artifact digest: %s != %s", firstDigest, secondDigest)
	}
}

func TestHashArtifactRejectsIgnoredRootFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry.pyc")
	if err := os.WriteFile(path, []byte("bytecode"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packageio.HashArtifact(context.Background(), path, 1<<20); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("HashArtifact(%q) = %v, want ErrInvalidFormat", path, err)
	}
}

func TestHashArtifactRejectsIgnoredRootDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ruff_cache")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "entry"), []byte("cache"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packageio.HashArtifact(context.Background(), path, 1<<20); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("HashArtifact(%q) = %v, want ErrInvalidFormat", path, err)
	}
}
