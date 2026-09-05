package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestBackupAndRestorePreserveCompleteSnapshot(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.db")
	backupPath := filepath.Join(directory, "backup.db")
	restorePath := filepath.Join(directory, "restored.db")
	store, err := sqlite.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	docs := store.PackageDocuments()
	scope := packstore.Scope{AppID: "campus-services", PackageID: "campus", Namespace: "campus/bus"}
	snapshot := map[string][]packstore.Document{
		"routes": {{ID: "route", Payload: []byte(`{"id":"route","name":"A-B","direction":"outbound","source_revision":"backup-revision"}`)}},
	}
	if err := docs.ReplaceSnapshot(t.Context(), scope, packstore.SnapshotMeta{
		Revision: "backup-revision", Source: "authorized-adapter", Authoritative: true,
		Complete: true, ImportedAt: now, ValidUntil: now.Add(time.Hour),
	}, snapshot); err != nil {
		t.Fatal(err)
	}
	historicalConfig, _, err := store.Ensure(t.Context(), validAppConfig("campus-services"))
	if err != nil {
		t.Fatal(err)
	}
	replacement := historicalConfig
	replacement.SystemPrompt = "备份恢复后的当前系统提示"
	currentConfig, err := store.CompareAndSwap(t.Context(), historicalConfig.Generation, replacement)
	if err != nil {
		t.Fatal(err)
	}
	createTestEchoRun(t, store, "campus-services", "backup-echo", "task", now)
	parent, err := store.ClaimRun(t.Context(), "campus-services", "backup-echo", "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	child := childRunRecord(parent, "backup-child", "backup-call", now)
	if err := store.CreateChildRun(t.Context(), parent, child, 1); err != nil {
		t.Fatal(err)
	}
	claimedChild, err := store.ClaimChildRun(t.Context(), child.AppID, child.EchoID, child.ID, parent.ID, "child-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteChildRun(t.Context(), claimedChild, kernelecho.RunStatusSucceeded, "备份子结果", publicerror.Error{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(t.Context(), parent, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, "备份父结果", publicerror.Error{}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(t.Context(), backupPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("备份权限=%v", info.Mode().Perm())
	}
	if err := sqlite.ValidateBackup(t.Context(), backupPath); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.RestoreBackup(t.Context(), backupPath, restorePath); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := sqlite.Open(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restoredDocs := restored.PackageDocuments()
	restoredListed, err := restoredDocs.List(t.Context(), scope, "routes", 10, "")
	if err != nil || !restoredListed.MetaFound || restoredListed.Meta.Revision != "backup-revision" {
		t.Fatalf("恢复后的快照元数据=%#v err=%v", restoredListed, err)
	}
	if len(restoredListed.Documents) != 1 || restoredListed.Documents[0].ID != "route" {
		t.Fatalf("恢复后的数据=%#v", restoredListed.Documents)
	}
	restoredCurrent, err := restored.Current(t.Context(), "campus-services")
	if err != nil || restoredCurrent.Revision != currentConfig.Revision {
		t.Fatalf("恢复后的当前 App 配置=%#v err=%v", restoredCurrent, err)
	}
	restoredHistorical, err := restored.Revision(t.Context(), "campus-services", historicalConfig.Revision)
	if err != nil || restoredHistorical.SystemPrompt != historicalConfig.SystemPrompt {
		t.Fatalf("恢复后的历史 App 配置=%#v err=%v", restoredHistorical, err)
	}
	restoredRuns, err := restored.ListRuns(t.Context(), "campus-services", "backup-echo")
	if err != nil || len(restoredRuns) != 2 {
		t.Fatalf("恢复后的 Run 链=%#v err=%v", restoredRuns, err)
	}
	for _, run := range restoredRuns {
		if run.ParentRunID != "" && (run.ParentRunID != parent.ID || run.ResultMessage != "备份子结果" ||
			len(run.CapabilityScope) != 1 || run.CapabilityScope[0] != "capability") {
			t.Fatalf("恢复后的子 Run=%#v", run)
		}
	}
}

func TestBackupAndRestoreNeverOverwriteExistingTargets(t *testing.T) {
	directory := t.TempDir()
	store, err := sqlite.Open(filepath.Join(directory, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	existing := filepath.Join(directory, "existing.db")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(t.Context(), existing); !errors.Is(err, sqlite.ErrBackupDestinationExists) {
		t.Fatalf("备份覆盖错误=%v", err)
	}
	backup := filepath.Join(directory, "backup.db")
	if err := store.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.RestoreBackup(t.Context(), backup, existing); !errors.Is(err, sqlite.ErrRestoreDestinationExists) {
		t.Fatalf("恢复覆盖错误=%v", err)
	}
	content, err := os.ReadFile(existing)
	if err != nil || string(content) != "keep" {
		t.Fatalf("既有目标被修改：%q err=%v", content, err)
	}
}

func TestConcurrentBackupPublicationHasExactlyOneWinner(t *testing.T) {
	directory := t.TempDir()
	store, err := sqlite.Open(filepath.Join(directory, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := filepath.Join(directory, "winner.db")
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- store.Backup(context.Background(), target)
		}()
	}
	wait.Wait()
	close(results)
	var succeeded, existed int
	for result := range results {
		switch {
		case result == nil:
			succeeded++
		case errors.Is(result, sqlite.ErrBackupDestinationExists):
			existed++
		default:
			t.Fatalf("并发备份错误=%v", result)
		}
	}
	if succeeded != 1 || existed != 1 {
		t.Fatalf("成功=%d 已存在=%d", succeeded, existed)
	}
	if err := sqlite.ValidateBackup(t.Context(), target); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBackupRejectsCorruptionAndCancelledOperation(t *testing.T) {
	directory := t.TempDir()
	corrupt := filepath.Join(directory, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.ValidateBackup(t.Context(), corrupt); !errors.Is(err, sqlite.ErrInvalidBackup) {
		t.Fatalf("损坏备份错误=%v", err)
	}

	store, err := sqlite.Open(filepath.Join(directory, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(directory, "cancelled.db")
	if err := store.Backup(cancelled, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消备份错误=%v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("取消后留下了发布文件：%v", err)
	}
}

func TestValidateBackupRejectsForgedMigrationHistoryWithoutRequiredSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forged.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
WITH RECURSIVE versions(value) AS (
  SELECT 1 UNION ALL SELECT value + 1 FROM versions WHERE value < 13
)
INSERT INTO schema_migrations(version,applied_at)
SELECT value,'2026-07-26T00:00:00Z' FROM versions;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.ValidateBackup(t.Context(), path); !errors.Is(err, sqlite.ErrInvalidBackup) {
		t.Fatalf("伪造迁移历史错误=%v", err)
	}
}
