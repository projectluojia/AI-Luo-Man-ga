package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

func internalTestRun(echoID, runID, groupID string, now time.Time) kernelecho.RunRecord {
	return kernelecho.RunRecord{
		ID: runID, RunGroupID: groupID, AppID: "app", EchoID: echoID,
		Attempt: 1, Status: kernelecho.RunStatusQueued,
		ExecutorID: "executor.test", ConfigRevision: "test-config", ProtocolVersion: "1.0",
		ExecutorConfig: json.RawMessage(`{"strategy":"test"}`), InputPayload: []byte("input"),
		InputContentType: "text/plain; charset=utf-8", MaxSteps: 4, MaxCapabilityCalls: 4,
		MaxExecutionUnits: 2000, MaxOutputBytes: 4096, ExecutionTimeoutMS: 5000,
		Deadline: now.Add(time.Minute), AvailableAt: now, RecoverableState: json.RawMessage(`{}`),
		CreatedAt: now,
	}
}

func insertInternalEchoRuns(t *testing.T, store *Store, echo kernelecho.Record, runs ...kernelecho.RunRecord) {
	t.Helper()
	tx, err := store.beginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
		store.txMu.Unlock()
	}()
	if len(runs) == 0 {
		t.Fatal("at least one run is required")
	}
	if err := insertEchoRun(context.Background(), tx, echo, runs[0]); err != nil {
		t.Fatal(err)
	}
	for _, run := range runs[1:] {
		if err := insertRun(context.Background(), tx, run); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	committed = true
}

func TestFinalizeEchoUsesPrimaryRunGroup(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "primary-group.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	echo := kernelecho.Record{ID: "echo", AppID: "app", InputMessage: "input", Status: kernelecho.StatusRunning, CreatedAt: now}
	root := internalTestRun(echo.ID, "root", "root-group", now)
	root.Status = kernelecho.RunStatusRunning
	root.LeaseToken = "root-lease"
	child := internalTestRun(echo.ID, "child", "child-group", now.Add(time.Second))
	child.ParentRunID = root.ID
	child.OriginCallID = "child-call"
	child.Attempt = 99
	child.Status = kernelecho.RunStatusSucceeded
	child.Result = kernelecho.Output{ContentType: "text/plain; charset=utf-8", Data: []byte("child result")}
	insertInternalEchoRuns(t, store, echo, root, child)

	if count, err := store.FailAbandonedRuns(t.Context(), "app", now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("FailAbandonedRuns count=%d err=%v", count, err)
	}
	record, _, err := store.GetEcho(t.Context(), "app", echo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusFailed || record.FinalMessage != "" {
		t.Fatalf("echo=%#v, want failed primary Run result", record)
	}
}

func TestLoadRunWorkFailsMalformedRunWithoutBlockingValidRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "malformed-run.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(t.Context(), "PRAGMA ignore_check_constraints = ON"); err != nil {
		t.Fatal(err)
	}
	defer store.db.ExecContext(context.Background(), "PRAGMA ignore_check_constraints = OFF")
	now := time.Now().UTC()
	echo := kernelecho.Record{ID: "echo", AppID: "app", InputMessage: "input", Status: kernelecho.StatusRunning, CreatedAt: now}
	valid := internalTestRun(echo.ID, "valid", "valid-group", now)
	malformed := internalTestRun(echo.ID, "malformed", "malformed-group", now)
	malformed.ParentRunID = valid.ID
	malformed.OriginCallID = "malformed-call"
	malformed.InputPayload = []byte{}
	malformed.InputContentType = ""
	insertInternalEchoRuns(t, store, echo, valid, malformed)

	work, err := store.ListQueuedRuns(t.Context(), "app", 10)
	if err != nil || len(work) != 1 || work[0].Run.ID != valid.ID {
		t.Fatalf("queued work=%#v err=%v", work, err)
	}
	failed, err := store.GetRun(t.Context(), "app", malformed.ID)
	if err != nil || failed.Status != kernelecho.RunStatusFailed {
		t.Fatalf("malformed run=%#v err=%v", failed, err)
	}
	record, _, err := store.GetEcho(t.Context(), "app", echo.ID)
	if err != nil || record.Status != kernelecho.StatusRunning {
		t.Fatalf("echo=%#v err=%v", record, err)
	}
}
