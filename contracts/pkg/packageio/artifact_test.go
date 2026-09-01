package packageio_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
