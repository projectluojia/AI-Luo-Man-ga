package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// TestContextMigration19AddsAssemblyColumns 验证迁移 19 为 runs 表增加会话上下文
// 与上下文版本列，并保持 schema 版本连续。
func TestContextMigration19AddsAssemblyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context-migration.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := db.QueryRow(`
SELECT count(*) FROM pragma_table_info('runs')
WHERE name IN ('session_id','user_id','message_id','context_digest','context_sources')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 5 {
		t.Fatalf("runs 表上下文列数量=%d，期望 5", columns)
	}
}

func setContextRun(t *testing.T, store *sqlite.Store) kernelecho.RunRecord {
	t.Helper()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "input", now)
	claimed, err := store.ClaimRun(context.Background(), "app", "echo", "lease-token", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestSetRunContextPersistsDigestAndSources(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := setContextRun(t, store)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sources := json.RawMessage(`{"config":{"version":"config-v1","count":1},"history":{"version":"h","count":2,"chars":10},"capabilities":{"version":"c","count":1}}`)
	if err := store.SetRunContext(context.Background(), run, digest, sources); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetRun(context.Background(), "app", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContextDigest != digest || string(loaded.ContextSources) != string(sources) {
		t.Fatalf("Run 上下文未持久化: %#v", loaded)
	}
}

func TestSetRunContextSetOncePerExecution(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := setContextRun(t, store)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := store.SetRunContext(context.Background(), run, digest, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	other := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := store.SetRunContext(context.Background(), run, other, json.RawMessage(`{}`)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("重复固化上下文错误=%v，期望 ErrInvalidTransition", err)
	}
}

func TestSetRunContextRejectsInvalidInput(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := setContextRun(t, store)
	cases := []struct {
		name    string
		digest  string
		sources json.RawMessage
	}{
		{"非法摘要", "short", json.RawMessage(`{}`)},
		{"非法摘要字符", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeZ", json.RawMessage(`{}`)},
		{"非法来源 JSON", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", json.RawMessage(`{`)},
		{"空来源", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := store.SetRunContext(context.Background(), run, test.digest, test.sources); !errors.Is(err, kernelecho.ErrInvalidRunRecord) {
				t.Fatalf("错误=%v，期望 ErrInvalidRunRecord", err)
			}
		})
	}
}

func TestRetryRunCarriesSessionContextAndResetsDigest(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "retry-context.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	_, run := echoRunRecords("app", "echo", "run-1", "input", now)
	run.SessionID = "session-1"
	run.UserID = "user-1"
	run.MessageID = "message-1"
	if err := store.CreateEchoRun(context.Background(), kernelecho.Record{
		ID: "echo", AppID: "app", InputMessage: "input",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRun(context.Background(), "app", "echo", "lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := store.SetRunContext(context.Background(), claimed, digest, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	next := claimed
	next.ID = "run-2"
	next.Attempt = 2
	next.Status = kernelecho.RunStatusQueued
	next.LeaseToken = ""
	next.LeaseExpiresAt = nil
	next.StartedAt = nil
	next.CompletedAt = nil
	next.ContextDigest = ""
	next.ContextSources = json.RawMessage(`{}`)
	next.Deadline = now.Add(2 * time.Minute)
	next.AvailableAt = now.Add(time.Minute)
	next.CreatedAt = now.Add(time.Minute)
	if err := store.RetryRun(context.Background(), claimed, next, publicerror.Echo("retry_test"), now); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), "app", "echo")
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	var retried kernelecho.RunRecord
	for _, item := range runs {
		if item.ID == "run-2" {
			retried = item
		}
	}
	if retried.SessionID != "session-1" || retried.UserID != "user-1" || retried.MessageID != "message-1" {
		t.Fatalf("重试 Run 丢失会话上下文: %#v", retried)
	}
	if retried.ContextDigest != "" || string(retried.ContextSources) != "{}" {
		t.Fatalf("重试 Run 未重置上下文版本: %#v", retried)
	}
}
