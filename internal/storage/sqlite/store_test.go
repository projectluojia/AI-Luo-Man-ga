package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func textOutput(value string) kernelecho.Output {
	return kernelecho.Output{ContentType: "text/plain; charset=utf-8", Data: []byte(value)}
}

func TestStorePersistsPackageDocumentsAndEchoAudit(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	docs := store.PackageDocuments()
	scope := packstore.Scope{AppID: "campus-services", Namespace: "campus/bus"}
	importedAt := time.Now().UTC()
	meta := packstore.SnapshotMeta{
		Revision: "rev-1", Source: "test", Authoritative: true,
		Complete: true, ImportedAt: importedAt, ValidUntil: importedAt.Add(24 * time.Hour),
	}
	collections := map[string][]packstore.Document{
		"routes": {{ID: "r", Payload: []byte(`{"id":"r","name":"文理—信息","direction":"去程","source_revision":"rev-1"}`)}},
	}
	if err := docs.ReplaceSnapshot(ctx, scope, meta, collections); err != nil {
		t.Fatal(err)
	}
	documents, err := docs.List(ctx, scope, "routes", 10, "")
	if err != nil || !documents.MetaFound || len(documents.Documents) != 1 || documents.Documents[0].ID != "r" {
		t.Fatalf("documents=%#v err=%v", documents, err)
	}
	if missing, err := docs.Get(ctx, packstore.Scope{AppID: "another-app", Namespace: "campus/bus"}, "routes", "r"); err != nil || missing.Found {
		t.Fatalf("cross-app read=%#v err=%v, want not found", missing, err)
	}
	now := time.Now().UTC()
	createTestEchoRun(t, store, "campus-services", "echo-1", "test", now)
	run, err := store.ClaimRun(ctx, "campus-services", "echo-1", "run-campus-services-echo-1", "lease-1", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendEchoEvent(ctx, kernelecho.Event{AppID: "campus-services", EchoID: "echo-1", RunID: run.ID, Type: "run.started", Payload: []byte(`{"ok":true}`), CreatedAt: now})
	if err != nil || event.Sequence != 1 {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	if err := store.CompleteRun(ctx, run, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, textOutput("done"), publicerror.Error{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record, events, err := store.GetEcho(ctx, "campus-services", "echo-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "done" || len(events) != 1 || events[0].RunID != run.ID {
		t.Fatalf("record=%#v events=%#v", record, events)
	}
	if err := store.RecordCapabilityCall(ctx, "call-1", run.ID, "echo-1", "campus-services", "campus.bus.test", []byte(`{"query":"redacted"}`), true, publicerror.Error{}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCapabilityCall(ctx, "call-1", run.ID, "echo-1", "campus-services", "campus.bus.test", []byte(`{"query":"redacted"}`), true, publicerror.Error{}, 2*time.Millisecond); err != nil {
		t.Fatalf("replay identical audit: %v", err)
	}
	if err := store.RecordCapabilityCall(ctx, "call-1", run.ID, "echo-1", "campus-services", "different.capability", []byte(`{"query":"redacted"}`), true, publicerror.Error{}, time.Millisecond); !errors.Is(err, idempotency.ErrKeyConflict) {
		t.Fatalf("conflicting audit error=%v, want ErrKeyConflict", err)
	}
	audits, err := store.ListCapabilityCalls(ctx, "campus-services", "echo-1")
	if err != nil || len(audits) != 1 || audits[0].AppID != "campus-services" || audits[0].CallID != "call-1" {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
}

func TestEchoCreationIdempotencyIsAtomicAndAppScoped(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "idempotent-create.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	echoA, runA := echoRunRecords("app-a", "echo-a", "run-a", "message", now)
	echoID, created, err := store.CreateEchoRunIdempotentLimited(ctx, "client-request", idempotency.Fingerprint([]byte("message")), echoA, runA, 0)
	if err != nil || !created || echoID != "echo-a" {
		t.Fatalf("first creation echo=%q created=%t err=%v", echoID, created, err)
	}
	replayEcho, replayRun := echoRunRecords("app-a", "must-not-exist", "must-not-exist", "message", now.Add(time.Second))
	echoID, created, err = store.CreateEchoRunIdempotentLimited(ctx, "client-request", idempotency.Fingerprint([]byte("message")), replayEcho, replayRun, 0)
	if err != nil || created || echoID != "echo-a" {
		t.Fatalf("replay echo=%q created=%t err=%v", echoID, created, err)
	}
	if _, _, err := store.GetEcho(ctx, "app-a", "must-not-exist"); !errors.Is(err, kernelecho.ErrEchoNotFound) {
		t.Fatalf("replay created a second Echo: %v", err)
	}
	if _, _, err := store.CreateEchoRunIdempotentLimited(ctx, "client-request", idempotency.Fingerprint([]byte("different")), replayEcho, replayRun, 0); !errors.Is(err, idempotency.ErrKeyConflict) {
		t.Fatalf("conflicting request error=%v, want ErrKeyConflict", err)
	}

	echoB, runB := echoRunRecords("app-b", "echo-b", "run-b", "different", now)
	echoID, created, err = store.CreateEchoRunIdempotentLimited(ctx, "client-request", idempotency.Fingerprint([]byte("different")), echoB, runB, 0)
	if err != nil || !created || echoID != "echo-b" {
		t.Fatalf("cross-App key creation echo=%q created=%t err=%v", echoID, created, err)
	}

	brokenEcho, brokenRun := echoRunRecords("app-a", "echo-broken", "run-a", "broken", now)
	if _, _, err := store.CreateEchoRunIdempotentLimited(ctx, "rollback-key", idempotency.Fingerprint([]byte("broken")), brokenEcho, brokenRun, 0); err == nil {
		t.Fatal("expected colliding Run identity to roll back")
	}
	validEcho, validRun := echoRunRecords("app-a", "echo-after-rollback", "run-after-rollback", "broken", now)
	if echoID, created, err := store.CreateEchoRunIdempotentLimited(ctx, "rollback-key", idempotency.Fingerprint([]byte("broken")), validEcho, validRun, 0); err != nil || !created || echoID != validEcho.ID {
		t.Fatalf("idempotency key was reserved by rolled-back creation: echo=%q created=%t err=%v", echoID, created, err)
	}
}

func TestRunQueueCapacityIsAtomicAndReplaySurvivesBackpressure(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "queue-capacity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const contenders = 12
	type result struct {
		key     string
		echoID  string
		created bool
		err     error
	}
	results := make(chan result, contenders)
	var workers sync.WaitGroup
	start := make(chan struct{})
	now := time.Now().UTC()
	for index := 0; index < contenders; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			key := fmt.Sprintf("capacity-%d", index)
			echoID := fmt.Sprintf("echo-%d", index)
			echo, run := echoRunRecords("app", echoID, "run-"+echoID, "input", now)
			stored, created, createErr := store.CreateEchoRunIdempotentLimited(
				context.Background(), key, idempotency.Fingerprint([]byte("input")), echo, run, 1,
			)
			results <- result{key: key, echoID: stored, created: created, err: createErr}
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner result
	successes := 0
	full := 0
	for item := range results {
		switch {
		case item.err == nil:
			successes++
			winner = item
		case errors.Is(item.err, kernelecho.ErrQueueFull):
			full++
		default:
			t.Fatalf("unexpected queue admission error: %v", item.err)
		}
	}
	if successes != 1 || full != contenders-1 || !winner.created {
		t.Fatalf("successes=%d full=%d winner=%#v", successes, full, winner)
	}
	replayEcho, replayRun := echoRunRecords("app", "replay-new-id", "replay-new-run", "input", now)
	stored, created, err := store.CreateEchoRunIdempotentLimited(
		context.Background(), winner.key, idempotency.Fingerprint([]byte("input")), replayEcho, replayRun, 1,
	)
	if err != nil || created || stored != winner.echoID {
		t.Fatalf("full-queue replay stored=%q created=%v err=%v", stored, created, err)
	}
	otherEcho, otherRun := echoRunRecords("other-app", "other", "other-run", "input", now)
	if _, created, err := store.CreateEchoRunIdempotentLimited(
		context.Background(), "other-capacity", idempotency.Fingerprint([]byte("input")), otherEcho, otherRun, 1,
	); err != nil || !created {
		t.Fatalf("App-scoped capacity rejected another App: created=%v err=%v", created, err)
	}
}

func TestDelayedRunAndLeaseRenewalAreGovernedByPersistentState(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "delayed-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	echo, run := echoRunRecords("app", "delayed", "delayed-run", "input", now)
	run.AvailableAt = now.Add(time.Minute)
	run.Deadline = run.AvailableAt.Add(time.Minute)
	if stored, created, err := store.CreateEchoRunIdempotentLimited(
		context.Background(), "delayed-echo", idempotency.Fingerprint([]byte("input")), echo, run, 0,
	); err != nil || !created || stored != echo.ID {
		t.Fatal(err)
	}
	if queued, err := store.ListQueuedRuns(context.Background(), "app", 10); err != nil || len(queued) != 1 {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if runnable, err := store.ListRunnableRuns(context.Background(), "app", now, 10); err != nil || len(runnable) != 0 {
		t.Fatalf("premature runnable=%#v err=%v", runnable, err)
	}
	if _, err := store.ClaimRun(context.Background(), "app", "delayed", "delayed-run", "early", now, now.Add(time.Minute)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("early claim error=%v", err)
	}
	startedAt := run.AvailableAt
	claimed, err := store.ClaimRun(context.Background(), "app", "delayed", "delayed-run", "lease", startedAt, startedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renewedExpiry := startedAt.Add(2 * time.Second)
	if err := store.RenewRunLease(context.Background(), claimed, startedAt.Add(500*time.Millisecond), renewedExpiry); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), "app", "delayed")
	if err != nil || len(runs) != 1 || runs[0].LeaseExpiresAt == nil || !runs[0].LeaseExpiresAt.Equal(renewedExpiry) {
		t.Fatalf("renewed runs=%#v err=%v", runs, err)
	}
	wrongLease := claimed
	wrongLease.LeaseToken = "wrong"
	if err := store.RenewRunLease(context.Background(), wrongLease, startedAt.Add(time.Second), startedAt.Add(3*time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("wrong lease renewal error=%v", err)
	}
	if err := store.RenewRunLease(context.Background(), claimed, renewedExpiry.Add(time.Second), renewedExpiry.Add(2*time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("expired lease renewal error=%v", err)
	}
}

func TestOrphanedQueuedRunIsDeterministicallyFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	echo, run := echoRunRecords("app", "orphan", "orphan-run", "input", now)
	if stored, created, err := store.CreateEchoRunIdempotentLimited(
		ctx, "orphan-echo", idempotency.Fingerprint([]byte("input")), echo, run, 0,
	); err != nil || !created || stored != echo.ID {
		t.Fatal(err)
	}
	// 模拟崩溃窗口：Echo 已进入终态而 Run 仍排队（绕过 CancelQueuedRun 的正常路径）。
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE echoes SET status=?,error_code='cancelled',error_message='Echo 已取消',completed_at=? WHERE app_id='app' AND echo_id='orphan'`,
		kernelecho.StatusCancelled, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// 调度读取不再返回孤儿，且该 Run 被确定性失败（不再永久占用 queued 状态）。
	work, err := store.ListRunnableRuns(ctx, "app", now, 10)
	if err != nil || len(work) != 0 {
		t.Fatalf("runnable=%#v err=%v", work, err)
	}
	runs, err := store.ListRuns(ctx, "app", "orphan")
	if err != nil || len(runs) != 1 || runs[0].Status != kernelecho.RunStatusFailed {
		t.Fatalf("孤儿 Run 未确定性失败: runs=%#v err=%v", runs, err)
	}
	if runs[0].ErrorCode != "recovery_failed" {
		t.Fatalf("孤儿 Run 错误码=%q，期望 recovery_failed", runs[0].ErrorCode)
	}
	if queued, err := store.ListQueuedRuns(ctx, "app", 10); err != nil || len(queued) != 0 {
		t.Fatalf("孤儿 Run 仍被列为 queued: queued=%#v err=%v", queued, err)
	}
}

func TestEchoAndAuditStorageEnforcesAppIsolation(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	claimed := make(map[string]kernelecho.RunRecord)
	for _, appID := range []string{"app-a", "app-b"} {
		createTestEchoRun(t, store, appID, "shared-echo", appID, now)
		run, err := store.ClaimRun(ctx, appID, "shared-echo", "run-"+appID+"-shared-echo", "lease-"+appID, now, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("claim %s: %v", appID, err)
		}
		claimed[appID] = run
		event, err := store.AppendEchoEvent(ctx, kernelecho.Event{
			AppID: appID, EchoID: "shared-echo", RunID: run.ID,
			Type: "run.started", Payload: []byte(`{"ok":true}`), CreatedAt: now,
		})
		if err != nil || event.Sequence != 1 {
			t.Fatalf("append %s event: %v", appID, err)
		}
		if err := store.RecordCapabilityCall(ctx, "shared-call", run.ID, "shared-echo", appID, "capability", []byte(`{"ok":true}`), true, publicerror.Error{}, time.Millisecond); err != nil {
			t.Fatalf("audit %s: %v", appID, err)
		}
	}

	if err := store.CompleteRun(ctx, claimed["app-a"], kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, textOutput("app-a-result"), publicerror.Error{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(ctx, claimed["app-b"], kernelecho.RunStatusFailed, kernelecho.StatusFailed, kernelecho.Output{}, publicerror.Error{Code: "sql_/srv/private.db", Message: "api-key-secret"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	crossAppRun := claimed["app-a"]
	crossAppRun.AppID = "app-c"
	if err := store.CompleteRun(ctx, crossAppRun, kernelecho.RunStatusFailed, kernelecho.StatusFailed, kernelecho.Output{}, publicerror.Error{Code: "internal_error"}, now.Add(time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("cross-app finish error=%v, want ErrInvalidTransition", err)
	}
	for _, appID := range []string{"app-a", "app-b"} {
		record, events, err := store.GetEcho(ctx, appID, "shared-echo")
		if err != nil {
			t.Fatalf("get %s: %v", appID, err)
		}
		if record.AppID != appID || record.InputMessage != appID || len(events) != 1 || events[0].AppID != appID || events[0].RunID != claimed[appID].ID {
			t.Fatalf("cross-app echo data for %s: record=%#v events=%#v", appID, record, events)
		}
		if appID == "app-b" && (record.ErrorCode != "internal_error" || record.ErrorMessage != "Echo 执行失败") {
			t.Fatalf("storage disclosed caller error: %#v", record)
		}
		audits, err := store.ListCapabilityCalls(ctx, appID, "shared-echo")
		if err != nil || len(audits) != 1 || audits[0].AppID != appID {
			t.Fatalf("cross-app audit data for %s: audits=%#v err=%v", appID, audits, err)
		}
	}
	if _, _, err := store.GetEcho(ctx, "app-c", "shared-echo"); !errors.Is(err, kernelecho.ErrEchoNotFound) {
		t.Fatalf("cross-app get error=%v, want ErrEchoNotFound", err)
	}
	audits, err := store.ListCapabilityCalls(ctx, "app-c", "shared-echo")
	if err != nil || len(audits) != 0 {
		t.Fatalf("cross-app audits leaked: audits=%#v err=%v", audits, err)
	}
	if _, err := store.AppendEchoEvent(ctx, kernelecho.Event{
		AppID: "app-c", EchoID: "shared-echo", RunID: claimed["app-a"].ID,
		Type: "run.started", Payload: []byte(`{}`), CreatedAt: now,
	}); err == nil {
		t.Fatal("cross-app event append unexpectedly succeeded")
	}
}

func TestRunStateMachineRejectsInvalidAndDuplicateTransitions(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "input", now)
	first, err := store.ClaimRun(ctx, "app", "echo", "run-app-echo", "lease-first", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, "app", "echo", "run-app-echo", "lease-duplicate", now, now.Add(time.Minute)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("duplicate claim error=%v", err)
	}
	wrongLease := first
	wrongLease.LeaseToken = "wrong"
	if err := store.CompleteRun(ctx, wrongLease, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, textOutput("bad"), publicerror.Error{}, now.Add(time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("wrong lease completion error=%v", err)
	}
	if err := store.CompleteRun(ctx, first, kernelecho.RunStatusSucceeded, kernelecho.StatusFailed, textOutput("bad"), publicerror.Error{}, now.Add(time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("invalid status pair error=%v", err)
	}
	if err := store.AdvanceRunExecutorSequence(ctx, first, 2); !errors.Is(err, kernelecho.ErrInvalidRunRecord) {
		t.Fatalf("sequence gap error=%v, want ErrInvalidRunRecord", err)
	}
	if err := store.AdvanceRunExecutorSequence(ctx, first, 1); err != nil {
		t.Fatalf("advance first Executor sequence: %v", err)
	}
	if err := store.AdvanceRunExecutorSequence(ctx, first, 1); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("duplicate Executor sequence error=%v, want ErrInvalidTransition", err)
	}
	first.LastExecutorSequence = 1
	if err := store.AdvanceRunExecutorSequenceWithUsage(ctx, first, 2, 12, 0, 1); err != nil {
		t.Fatalf("advance second executor sequence with usage: %v", err)
	}
	if err := store.AdvanceRunExecutorSequenceWithUsage(ctx, first, 2, 12, 0, 1); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("duplicate usage update error=%v, want ErrInvalidTransition", err)
	}
	first.LastExecutorSequence = 2
	first.UsedExecutionUnits = 12
	first.UsedRetries = 1
	if err := store.AdvanceRunExecutorSequenceWithUsage(ctx, first, 3, 11, 0, 1); !errors.Is(err, kernelecho.ErrInvalidRunRecord) {
		t.Fatalf("inconsistent usage error=%v, want ErrInvalidRunRecord", err)
	}
	if err := store.AdvanceRunExecutorSequenceWithUsage(ctx, first, 3, 2001, 0, 1); !errors.Is(err, kernelecho.ErrInvalidRunRecord) {
		t.Fatalf("over-budget usage error=%v, want ErrInvalidRunRecord", err)
	}
	firstEvent, err := store.AppendEchoEvent(ctx, kernelecho.Event{
		AppID: "app", EchoID: "echo", RunID: first.ID, Type: "run.started",
		Payload: []byte(`{}`), CreatedAt: now,
	})
	if err != nil || firstEvent.Sequence != 1 {
		t.Fatalf("first event=%#v err=%v", firstEvent, err)
	}
	next := first
	next.ID = "run-app-echo-attempt-2"
	next.Attempt = 2
	next.Status = kernelecho.RunStatusQueued
	next.LeaseToken = ""
	next.LeaseExpiresAt = nil
	next.StartedAt = nil
	next.CompletedAt = nil
	next.ErrorCode = ""
	next.ErrorMessage = ""
	next.LastExecutorSequence = 0
	next.UsedExecutionUnits = 0
	next.UsedCostMicrousd = 0
	next.UsedRetries = 0
	next.CreatedAt = now.Add(2 * time.Second)
	next.AvailableAt = next.CreatedAt
	next.Deadline = now.Add(time.Minute)
	if err := store.RetryRun(ctx, first, next, publicerror.Error{Code: "executor_unavailable"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryRun(ctx, first, next, publicerror.Error{Code: "executor_unavailable"}, now.Add(time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("duplicate retry error=%v", err)
	}
	second, err := store.ClaimRun(ctx, "app", "echo", "run-app-echo-attempt-2", "lease-second", now.Add(2*time.Second), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	secondEvent, err := store.AppendEchoEvent(ctx, kernelecho.Event{
		AppID: "app", EchoID: "echo", RunID: second.ID, Type: "run.started",
		Payload: []byte(`{}`), CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil || secondEvent.Sequence != 2 {
		t.Fatalf("second event=%#v err=%v", secondEvent, err)
	}
	if err := store.CompleteRun(ctx, second, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, textOutput("done"), publicerror.Error{}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(ctx, second, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, textOutput("done-again"), publicerror.Error{}, now.Add(4*time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("duplicate terminal completion error=%v", err)
	}
	record, events, err := store.GetEcho(ctx, "app", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "done" || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("record=%#v events=%#v", record, events)
	}
	runs, err := store.ListRuns(ctx, "app", "echo")
	if err != nil || len(runs) != 2 || runs[0].Status != kernelecho.RunStatusFailed || runs[1].Status != kernelecho.RunStatusSucceeded {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
}

func TestQueuedCancellationAndAbandonedRunReconciliationAreDurable(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "queued", "queued input", now)
	cancelled, err := store.CancelQueuedRun(ctx, "app", "queued", now.Add(time.Second))
	if err != nil || !cancelled {
		t.Fatalf("cancelled=%t err=%v", cancelled, err)
	}
	if _, err := store.ClaimRun(ctx, "app", "queued", "run-app-queued", "lease", now, now.Add(time.Minute)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("cancelled Run claim error=%v", err)
	}
	queuedEcho, _, err := store.GetEcho(ctx, "app", "queued")
	if err != nil || queuedEcho.Status != kernelecho.StatusCancelled {
		t.Fatalf("queued Echo=%#v err=%v", queuedEcho, err)
	}

	createTestEchoRun(t, store, "app", "abandoned", "abandoned input", now)
	if _, err := store.ClaimRun(ctx, "app", "abandoned", "run-app-abandoned", "old-process-lease", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	createTestEchoRun(t, store, "app", "recoverable", "recover me", now)
	failed, err := store.FailAbandonedRuns(ctx, "app", now.Add(2*time.Second))
	if err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	abandoned, _, err := store.GetEcho(ctx, "app", "abandoned")
	if err != nil || abandoned.Status != kernelecho.StatusFailed || abandoned.ErrorCode != "recovery_failed" {
		t.Fatalf("abandoned Echo=%#v err=%v", abandoned, err)
	}
	work, err := store.ListQueuedRuns(ctx, "app", 10)
	if err != nil || len(work) != 1 || work[0].Run.EchoID != "recoverable" || string(work[0].Run.InputPayload) != "recover me" {
		t.Fatalf("work=%#v err=%v", work, err)
	}
}

func TestCancelQueuedRunsCancelsDelayedRuns(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "cancel-queued-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	echo, run := echoRunRecords("app-a", "delayed", "delayed-run", "delayed", now)
	run.AvailableAt = now.Add(time.Hour)
	run.Deadline = run.AvailableAt.Add(time.Minute)
	if stored, created, err := store.CreateEchoRunIdempotentLimited(
		ctx, "cancel-delayed", idempotency.Fingerprint([]byte("delayed")), echo, run, 0,
	); err != nil || !created || stored != echo.ID {
		t.Fatalf("create delayed Echo/Run: %v", err)
	}
	otherEcho, otherRun := echoRunRecords("app-b", "delayed", "delayed-run-b", "delayed", now)
	otherRun.AvailableAt = now.Add(time.Hour)
	otherRun.Deadline = otherRun.AvailableAt.Add(time.Minute)
	if stored, created, err := store.CreateEchoRunIdempotentLimited(
		ctx, "cancel-delayed-other", idempotency.Fingerprint([]byte("delayed-other")), otherEcho, otherRun, 0,
	); err != nil || !created || stored != otherEcho.ID {
		t.Fatalf("create other-app delayed Echo/Run: %v", err)
	}
	if err := store.CancelQueuedRuns(ctx, "app-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListQueuedRuns(ctx, "app-a", 10)
	if err != nil || len(queued) != 0 {
		t.Fatalf("queued=%#v err=%v, want no queued Run", queued, err)
	}
	record, _, err := store.GetEcho(ctx, "app-a", "delayed")
	if err != nil || record.Status != kernelecho.StatusCancelled {
		t.Fatalf("record=%#v err=%v, want cancelled Echo", record, err)
	}
	runs, err := store.ListRuns(ctx, "app-a", "delayed")
	if err != nil || len(runs) != 1 || runs[0].Status != kernelecho.RunStatusCancelled {
		t.Fatalf("runs=%#v err=%v, want cancelled Run", runs, err)
	}
	if queued, err := store.ListQueuedRuns(ctx, "app-b", 10); err != nil || len(queued) != 1 {
		t.Fatalf("cross-app queued=%#v err=%v, want untouched queued Run", queued, err)
	}
}

func TestEchoAndRunCreationRollsBackAtomically(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "first", "one", now)
	run := kernelecho.RunRecord{
		ID: "run-app-first", RunGroupID: "run-app-first", AppID: "app", EchoID: "second", Attempt: 1,
		Status: kernelecho.RunStatusQueued, ExecutorID: "executor.test", ExecutorConfig: []byte(`{"strategy":"test"}`), ConfigRevision: "test-config-v1",
		InputPayload: []byte("two"), InputContentType: "text/plain; charset=utf-8",
		ProtocolVersion: "1.0", MaxSteps: 4, MaxCapabilityCalls: 4,
		MaxExecutionUnits: 2000,
		MaxOutputBytes:    4096, ExecutionTimeoutMS: 5000, Deadline: now.Add(time.Minute), AvailableAt: now,
		RecoverableState: []byte(`{}`), CreatedAt: now,
	}
	_, _, err = store.CreateEchoRunIdempotentLimited(context.Background(), "atomic-second", idempotency.Fingerprint([]byte("two")), kernelecho.Record{
		ID: "second", AppID: "app", InputMessage: "two",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, run, 0)
	if err == nil {
		t.Fatal("duplicate Run identity unexpectedly succeeded")
	}
	if _, _, err := store.GetEcho(context.Background(), "app", "second"); !errors.Is(err, kernelecho.ErrEchoNotFound) {
		t.Fatalf("Echo creation was not rolled back: %v", err)
	}
}

func createTestEchoRun(t *testing.T, store *sqlite.Store, appID, echoID, input string, createdAt time.Time) kernelecho.RunRecord {
	t.Helper()
	echo, run := echoRunRecords(appID, echoID, "run-"+appID+"-"+echoID, input, createdAt)
	if stored, created, err := store.CreateEchoRunIdempotentLimited(
		context.Background(), "test-"+echo.ID, idempotency.Fingerprint([]byte(echo.InputMessage)), echo, run, 0,
	); err != nil || !created || stored != echo.ID {
		t.Fatalf("create Echo/Run: %v", err)
	}
	return run
}

func echoRunRecords(appID, echoID, runID, input string, createdAt time.Time) (kernelecho.Record, kernelecho.RunRecord) {
	run := kernelecho.RunRecord{
		ID:                 runID,
		RunGroupID:         runID,
		AppID:              appID,
		EchoID:             echoID,
		Attempt:            1,
		Status:             kernelecho.RunStatusQueued,
		ExecutorID:         "executor.test",
		ExecutorConfig:     []byte(`{"strategy":"test"}`),
		InputPayload:       []byte(input),
		InputContentType:   "text/plain; charset=utf-8",
		ConfigRevision:     "test-config-v1",
		ProtocolVersion:    "1.0",
		MaxSteps:           4,
		MaxCapabilityCalls: 4,
		MaxExecutionUnits:  2000,
		MaxOutputBytes:     4096,
		ExecutionTimeoutMS: 5000,
		Deadline:           createdAt.Add(time.Minute),
		AvailableAt:        createdAt,
		RecoverableState:   []byte(`{}`),
		CreatedAt:          createdAt,
	}
	return kernelecho.Record{
		ID: echoID, AppID: appID, InputMessage: input,
		Status: kernelecho.StatusRunning, CreatedAt: createdAt,
	}, run
}
