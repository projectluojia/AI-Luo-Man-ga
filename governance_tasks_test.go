package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func openSweepFixture(t *testing.T) (*sqlite.Store, *confirmation.Service, *task.Scheduler, *task.TypeRegistry) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "governance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateEchoRun(ctx, kernelecho.Record{
		ID: "echo-1", AppID: campus.AppID, InputMessage: "test-input",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: "run-1", RunGroupID: "run-1", AppID: campus.AppID, EchoID: "echo-1",
		Attempt: 1, Status: kernelecho.RunStatusQueued,
		Model: "test-model", ModelConfigVersion: "v1", ProtocolVersion: "1.0",
		MaxSteps: 8, MaxToolCalls: 4, MaxInputTokens: 4096, MaxOutputTokens: 2048,
		MaxTotalTokens: 8192, MaxOutputBytes: 65536, MaxCostMicrousd: 0,
		ProviderTimeoutMS: 5000,
		Deadline:          now.Add(time.Hour),
		AvailableAt:       now,
		CreatedAt:         now,
		RecoverableState:  json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	confirmations := confirmation.NewService(store)
	types := task.NewTypeRegistry()
	scheduler := task.NewScheduler(store, types, task.Config{})
	if err := registerGovernanceTaskTypes(types, confirmations, scheduler, confirmationSweepInterval); err != nil {
		t.Fatal(err)
	}
	return store, confirmations, scheduler, types
}

// insertExpiredConfirmation 直接落一条已过期的 waiting 确认（Request 会拒绝过去时间，
// 这里绕过服务直接写存储，模拟"进程停机期间确认自然过期"的真实场景）。
func insertExpiredConfirmation(t *testing.T, store *sqlite.Store) {
	t.Helper()
	created := time.Now().UTC().Add(-10 * time.Minute)
	digest, err := confirmation.Digest([]byte(`{"message":"发车提醒"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Create(context.Background(), confirmation.Confirmation{
		AppID:          campus.AppID,
		ConfirmationID: "confirmation-1",
		EchoID:         "echo-1",
		RunID:          "run-1",
		CallID:         "call-1",
		CapabilityID:   "campus.bus.notify",
		TargetType:     confirmation.TargetTypeCapability,
		TargetID:       "campus.bus.notify",
		SideEffect:     confirmation.SideEffectExternal,
		IdempotencyKey: "operation-1",
		ArgumentDigest: digest,
		Status:         confirmation.StatusWaiting,
		ExpiresAt:      created.Add(time.Minute), // 已在过去：清扫应将其过期
		CreatedAt:      created,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestConfirmationSweepExpiresDueAndReschedules 验证清扫处理器：已到期确认被标记
// expired，且无条件安排下一轮（清扫链不因单次成败中断）。
func TestConfirmationSweepExpiresDueAndReschedules(t *testing.T) {
	store, confirmations, _, types := openSweepFixture(t)
	ctx := context.Background()
	insertExpiredConfirmation(t, store)

	spec, ok := types.Lookup(confirmationSweepType)
	if !ok {
		t.Fatal("清扫任务类型未注册")
	}
	now := time.Now().UTC()
	execution := task.Task{
		AppID: campus.AppID, TaskID: "sweep-1", Type: confirmationSweepType,
		Status: task.StatusRunning, Attempt: 1, MaxAttempts: 3,
		Deadline: now.Add(time.Hour), AvailableAt: now,
		IdempotencyKey: "confirmation.expiry.test",
		Params:         sweepParams,
		CreatedAt:      now, UpdatedAt: now,
	}
	if err := spec.Handler(ctx, execution); err != nil {
		t.Fatalf("清扫处理器执行失败: %v", err)
	}
	// 已到期确认被标记过期。
	if _, err := confirmations.Resolve(ctx, campus.AppID, "confirmation-1"); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("清扫后 Resolve 得到 %v, want ErrExpired", err)
	}
	// 下一轮清扫任务已入队，清扫链不断。
	queued, err := store.ListTasks(ctx, campus.AppID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Type != confirmationSweepType || queued[0].Status != task.StatusQueued {
		t.Fatalf("下一轮清扫任务=%#v err=%v, want 一条 queued 清扫任务", queued, err)
	}
	// 下一轮不是立即执行，而是按周期延后。
	if !queued[0].AvailableAt.After(now) {
		t.Fatalf("下一轮清扫 available_at=%s 应晚于当前时间", queued[0].AvailableAt)
	}
}

// TestSeedConfirmationSweep 验证启动播种：首轮清扫任务在 interval 之后才到期。
func TestSeedConfirmationSweep(t *testing.T) {
	store, _, scheduler, _ := openSweepFixture(t)
	ctx := context.Background()
	if err := seedConfirmationSweep(ctx, scheduler, campus.AppID, confirmationSweepInterval); err != nil {
		t.Fatalf("播种清扫任务: %v", err)
	}
	now := time.Now().UTC()
	queued, err := store.ListTasks(ctx, campus.AppID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Type != confirmationSweepType {
		t.Fatalf("播种结果=%#v, want 一条清扫任务", queued)
	}
	// 首轮在 interval 后执行，进程刚启动不立刻清扫。
	if !queued[0].AvailableAt.After(now.Add(confirmationSweepInterval - time.Minute)) {
		t.Fatalf("首轮清扫 available_at=%s 应约在 %s 之后", queued[0].AvailableAt, now.Add(confirmationSweepInterval))
	}
}
