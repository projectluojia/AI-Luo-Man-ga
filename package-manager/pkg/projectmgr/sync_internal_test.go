package projectmgr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverSyncTransactionRestoresPublishedState(t *testing.T) {
	projectDir := t.TempDir()
	installRoot := filepath.Join(projectDir, "runtime")
	backupRoot := filepath.Join(projectDir, ".ailuo-root-backup-test")
	stageRoot := filepath.Join(projectDir, ".ailuo-stage-test")
	transactionDir := filepath.Join(projectDir, ".ailuo-sync-transaction-test")
	if err := os.MkdirAll(installRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installRoot, "state"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, "state"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(transactionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(projectDir, "ailuo.lock")
	lockBackupPath := filepath.Join(transactionDir, "project-lock.backup")
	if err := os.WriteFile(lockPath, []byte("new-lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockBackupPath, []byte("old-lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(projectDir, ".ailuo-sync-transaction.json")
	journalBytes, err := json.Marshal(syncJournal{
		SchemaVersion: syncJournalSchemaVersion, TransactionDir: transactionDir,
		LockPath: lockPath, LockBackupPath: lockBackupPath, HadLock: true,
		InstallRoot: installRoot, StageRoot: stageRoot, BackupRoot: backupRoot,
		Phase: "root_published",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSyncedFile(journalPath, journalBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverSyncTransaction(context.Background(), journalPath, installRoot, lockPath); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(installRoot, "state"))
	if err != nil || string(state) != "old" {
		t.Fatalf("state=%q err=%v, want old state", state, err)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil || string(lock) != "old-lock" {
		t.Fatalf("lock=%q err=%v, want old lock", lock, err)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal still exists: %v", err)
	}
}

func TestSyncTransactionRollbackRestoresPublishedState(t *testing.T) {
	projectDir := t.TempDir()
	installRoot := filepath.Join(projectDir, "runtime")
	stageRoot := filepath.Join(projectDir, ".stage")
	transactionPath := filepath.Join(projectDir, ".ailuo-sync-transaction.json")
	lockPath := filepath.Join(projectDir, "ailuo.lock")
	if err := os.MkdirAll(installRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stageRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installRoot, "state"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRoot, "state"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("old-lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	transaction, err := newSyncTransaction(transactionPath, lockPath, installRoot, stageRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(stageRoot); _ = transaction.cleanup() }()
	published, err := reserveInstallRootPublication(installRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(installRoot, published.backup); err != nil {
		t.Fatal(err)
	}
	published.backedUp = true
	if err := os.Rename(stageRoot, installRoot); err != nil {
		t.Fatal(err)
	}
	published.published = true
	if err := writeSyncedFile(lockPath, []byte("new-lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.rollback(published); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(installRoot, "state"))
	if err != nil || string(state) != "old" {
		t.Fatalf("state=%q err=%v, want old state", state, err)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil || string(lock) != "old-lock" {
		t.Fatalf("lock=%q err=%v, want old lock", lock, err)
	}
}
