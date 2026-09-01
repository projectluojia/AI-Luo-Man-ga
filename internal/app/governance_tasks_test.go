package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

const testAppID = "test-app"

func openSweepFixture(t *testing.T) (*sqlite.Store, *confirmation.Service, *task.Scheduler, *task.TypeRegistry) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "governance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	if _, created, err := store.CreateEchoRunIdempotentLimited(ctx, "governance-echo", idempotency.Fingerprint([]byte("test-input")), kernelecho.Record{
		ID: "echo-1", AppID: testAppID, InputMessage: "test-input", Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: "run-1", RunGroupID: "run-1", AppID: testAppID, EchoID: "echo-1", Attempt: 1, Status: kernelecho.RunStatusQueued,
		ExecutorID: "executor.test", ExecutorConfig: json.RawMessage(`{"strategy":"test"}`), ConfigRevision: "v1", ProtocolVersion: "1.0", MaxSteps: 8, MaxCapabilityCalls: 4,
		InputPayload: []byte("test-input"), InputContentType: "text/plain; charset=utf-8",
		MaxExecutionUnits: 8192, MaxOutputBytes: 65536,
		ExecutionTimeoutMS: 5000, Deadline: now.Add(time.Hour), AvailableAt: now, CreatedAt: now,
		RecoverableState: json.RawMessage(`{}`),
	}, 0); err != nil || !created {
		t.Fatal(err)
	}
	confirmations := confirmation.NewService(store, confirmation.Config{})
	types := task.NewTypeRegistry()
	scheduler := task.NewScheduler(store, types, task.Config{})
	if err := registerGovernanceTaskTypes(types, confirmations, scheduler, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	return store, confirmations, scheduler, types
}

func insertExpiredConfirmation(t *testing.T, store *sqlite.Store) {
	t.Helper()
	created := time.Now().UTC().Add(-10 * time.Minute)
	digest, err := confirmation.Digest([]byte(`{"message":"发车提醒"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), confirmation.Confirmation{
		AppID: testAppID, ConfirmationID: "confirmation-1", EchoID: "echo-1", RunID: "run-1", CallID: "call-1",
		CapabilityID: "test.notify", TargetType: confirmation.TargetTypeCapability, TargetID: "test.notify",
		SideEffect: confirmation.SideEffectExternal, IdempotencyKey: "operation-1", ArgumentDigest: digest,
		Status: confirmation.StatusWaiting, ExpiresAt: created.Add(time.Minute), CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmationSweepExpiresDueAndReschedules(t *testing.T) {
	store, confirmations, _, types := openSweepFixture(t)
	insertExpiredConfirmation(t, store)
	spec, ok := types.Lookup(confirmationSweepType)
	if !ok {
		t.Fatal("清扫任务类型未注册")
	}
	now := time.Now().UTC()
	execution := task.Task{
		AppID: testAppID, TaskID: "sweep-1", Type: confirmationSweepType, Status: task.StatusRunning,
		Attempt: 1, MaxAttempts: 3, Deadline: now.Add(time.Hour), AvailableAt: now,
		IdempotencyKey: "confirmation.expiry.test", Params: sweepParams, CreatedAt: now, UpdatedAt: now,
	}
	if err := spec.Handler(context.Background(), execution); err != nil {
		t.Fatalf("清扫处理器执行失败: %v", err)
	}
	if _, err := confirmations.Resolve(context.Background(), testAppID, "confirmation-1"); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("清扫后 Resolve 得到 %v, want ErrExpired", err)
	}
	queued, err := store.ListTasks(context.Background(), testAppID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Type != confirmationSweepType || queued[0].Status != task.StatusQueued {
		t.Fatalf("下一轮清扫任务=%#v", queued)
	}
	if !queued[0].AvailableAt.After(now) {
		t.Fatalf("下一轮清扫 available_at=%s 应晚于当前时间", queued[0].AvailableAt)
	}
}

func TestSeedConfirmationSweep(t *testing.T) {
	store, _, scheduler, _ := openSweepFixture(t)
	interval := 5 * time.Minute
	if err := seedConfirmationSweep(context.Background(), scheduler, testAppID, interval); err != nil {
		t.Fatalf("播种清扫任务: %v", err)
	}
	queued, err := store.ListTasks(context.Background(), testAppID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Type != confirmationSweepType {
		t.Fatalf("播种结果=%#v", queued)
	}
}
