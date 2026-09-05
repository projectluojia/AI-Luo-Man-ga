package packmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishStageRestoresOldDirectoryWhenPublishRenameFails(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo.pkg")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(target, "old.txt")
	if err := os.WriteFile(old, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, ".stage-missing")
	if _, err := publishStage(context.Background(), root, target, stage); err == nil {
		t.Fatal("publishStage succeeded for missing stage")
	}
	content, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("old installation was not restored: %v", err)
	}
	if string(content) != "old" {
		t.Fatalf("old installation content = %q", content)
	}
}
