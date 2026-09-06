package packageio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

func TestFileLockIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.lock")
	first, err := packageio.AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatalf("AcquireFileLock: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "\n") {
		t.Fatalf("lock contents=%q err=%v, want owner marker", data, err)
	}
	second, err := packageio.AcquireFileLock(context.Background(), path)
	if second != nil || !errors.Is(err, packageio.ErrFileLocked) {
		t.Fatalf("second AcquireFileLock=%v,%v, want ErrFileLocked", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock after Close err=%v, want not exist", err)
	}
	third, err := packageio.AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatalf("AcquireFileLock after Close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestFileLockDoesNotAutomaticallyRemoveStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.lock")
	if err := os.WriteFile(path, []byte("unknown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := packageio.AcquireFileLock(context.Background(), path); !errors.Is(err, packageio.ErrFileLocked) {
		t.Fatalf("AcquireFileLock with existing lock=%v, want ErrFileLocked", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("existing lock was removed: %v", err)
	}
	if err := packageio.RemoveFileLock(path); err != nil {
		t.Fatalf("RemoveFileLock: %v", err)
	}
}

func TestAcquireFileLockHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := packageio.AcquireFileLock(ctx, filepath.Join(t.TempDir(), "operation.lock")); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireFileLock canceled=%v, want context.Canceled", err)
	}
}
