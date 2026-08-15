package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestChildRunStateIsDurableScopedAndDoesNotCompleteEcho(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-run.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "task", now)
	parent, err := store.ClaimRun(ctx, "app", "echo", "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	child := childRunRecord(parent, "child", "call", now)
	if err := store.CreateChildRun(ctx, parent, child); err != nil {
		t.Fatal(err)
	}
	if work, err := store.ListQueuedRuns(ctx, "app", 10); err != nil || len(work) != 1 || work[0].InputMessage != "child task" {
		t.Fatalf("queued child work=%#v err=%v", work, err)
	}
	if _, err := store.GetRun(ctx, "other-app", child.ID); !errors.Is(err, kernelecho.ErrRunNotFound) {
		t.Fatalf("cross-App child read error=%v", err)
	}
	claimed, err := store.ClaimChildRun(ctx, "app", "echo", child.ID, parent.ID, "child-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEchoEvent(ctx, kernelecho.Event{
		AppID: "app", EchoID: "echo", RunID: claimed.ID, Type: "reply.delta",
		Payload: []byte(`{"text":"private"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("child event error=%v", err)
	}
	if err := store.CompleteRun(ctx, parent, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, "done", publicerror.Error{}, now.Add(time.Second)); err != nil {
		t.Fatalf("parent completion error=%v", err)
	}
	if err := store.RecordCapabilityCall(ctx, "same-call", parent.ID, "echo", "app", "capability", []byte(`{"root":true}`), true, publicerror.Error{}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCapabilityCall(ctx, "same-call", claimed.ID, "echo", "app", "capability", []byte(`{"child":true}`), true, publicerror.Error{}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteChildRun(ctx, claimed, kernelecho.RunStatusSucceeded, "child result", publicerror.Error{}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	echoRecord, _, err := store.GetEcho(ctx, "app", "echo")
	if err != nil || echoRecord.Status != kernelecho.StatusSucceeded || echoRecord.FinalMessage != "done" {
		t.Fatalf("Echo=%#v err=%v", echoRecord, err)
	}
	storedChild, err := store.GetRun(ctx, "app", child.ID)
	if err != nil || storedChild.ParentRunID != parent.ID || storedChild.OriginCallID != "call" ||
		storedChild.ResultMessage != "child result" || storedChild.Status != kernelecho.RunStatusSucceeded {
		t.Fatalf("child=%#v err=%v", storedChild, err)
	}
	audits, err := store.ListCapabilityCalls(ctx, "app", "echo")
	if err != nil || len(audits) != 2 || audits[0].RunID == audits[1].RunID {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
}

func TestChildRunEnforcesOneLevelOneChildAndParentLease(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "task", now)
	parent, err := store.ClaimRun(ctx, "app", "echo", "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wrongLease := parent
	wrongLease.LeaseToken = "wrong"
	if err := store.CreateChildRun(ctx, wrongLease, childRunRecord(parent, "child-a", "call-a", now)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("wrong parent lease error=%v", err)
	}
	first := childRunRecord(parent, "child-a", "call-a", now)
	if err := store.CreateChildRun(ctx, parent, first); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateChildRun(ctx, parent, childRunRecord(parent, "child-b", "call-b", now)); !errors.Is(err, kernelecho.ErrChildRunLimit) {
		t.Fatalf("second child error=%v", err)
	}
	claimed, err := store.ClaimChildRun(ctx, "app", "echo", first.ID, parent.ID, "child-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nested := childRunRecord(claimed, "nested", "nested-call", now)
	if err := store.CreateChildRun(ctx, claimed, nested); !errors.Is(err, kernelecho.ErrInvalidRunRecord) {
		t.Fatalf("nested child error=%v", err)
	}
}

func TestQueuedChildClaimFailureCanBeClosedDurably(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-claim-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "task", now)
	parent, err := store.ClaimRun(ctx, "app", "echo", "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	child := childRunRecord(parent, "child", "call", now)
	if err := store.CreateChildRun(ctx, parent, child); err != nil {
		t.Fatal(err)
	}
	failure := publicerror.Echo("recovery_failed")
	if err := store.FailQueuedChildRun(ctx, child, failure, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	storedChild, err := store.GetRun(ctx, "app", child.ID)
	if err != nil || storedChild.Status != kernelecho.RunStatusFailed ||
		storedChild.ErrorCode != failure.Code || storedChild.ResultMessage != "" {
		t.Fatalf("child=%#v err=%v", storedChild, err)
	}
	if err := store.FailQueuedChildRun(ctx, child, failure, now.Add(2*time.Second)); !errors.Is(err, kernelecho.ErrInvalidTransition) {
		t.Fatalf("重复清理错误=%v", err)
	}
	if err := store.CompleteRun(ctx, parent, kernelecho.RunStatusFailed, kernelecho.StatusFailed, "", failure, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryFailsRunningRootAndOrphanChildAtomically(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	createTestEchoRun(t, store, "app", "echo", "task", now)
	parent, err := store.ClaimRun(ctx, "app", "echo", "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	child := childRunRecord(parent, "child", "call", now)
	if err := store.CreateChildRun(ctx, parent, child); err != nil {
		t.Fatal(err)
	}
	failed, err := store.FailAbandonedRuns(ctx, "app", now.Add(time.Minute))
	if err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	storedParent, err := store.GetRun(ctx, "app", parent.ID)
	if err != nil || storedParent.Status != kernelecho.RunStatusFailed || storedParent.ErrorCode != "recovery_failed" {
		t.Fatalf("parent=%#v err=%v", storedParent, err)
	}
	storedChild, err := store.GetRun(ctx, "app", child.ID)
	if err != nil || storedChild.Status != kernelecho.RunStatusQueued || storedChild.TaskMessage != "child task" {
		t.Fatalf("child=%#v err=%v", storedChild, err)
	}
	echoRecord, _, err := store.GetEcho(ctx, "app", "echo")
	if err != nil || echoRecord.Status != kernelecho.StatusRunning {
		t.Fatalf("Echo=%#v err=%v", echoRecord, err)
	}
}

func childRunRecord(parent kernelecho.RunRecord, runID, callID string, now time.Time) kernelecho.RunRecord {
	return kernelecho.RunRecord{
		ID: runID, RunGroupID: runID, AppID: parent.AppID, EchoID: parent.EchoID,
		ParentRunID: parent.ID, OriginCallID: callID, Attempt: 1, Status: kernelecho.RunStatusQueued,
		TaskMessage: "child task",
		Model:       parent.Model, ModelConfigVersion: parent.ModelConfigVersion, ProtocolVersion: parent.ProtocolVersion,
		MaxSteps: 2, MaxToolCalls: 2, MaxInputTokens: 500, MaxOutputTokens: 500, MaxTotalTokens: 1000,
		MaxOutputBytes: 2048, ProviderTimeoutMS: 2500,
		Deadline: now.Add(30 * time.Second), AvailableAt: now,
		CapabilityScope: []string{"capability"}, PermissionScope: []string{},
		RecoverableState: []byte(`{}`), CreatedAt: now,
	}
}
