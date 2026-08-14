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

	_ "modernc.org/sqlite"

	kerneltask "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// newTaskRecord 构造一个通过内核校验的新任务记录。
func newTaskRecord(appID, taskID string, now time.Time) kerneltask.Task {
	return kerneltask.Task{
		AppID:          appID,
		TaskID:         taskID,
		Type:           "bus.catalog.sync",
		Status:         kerneltask.StatusQueued,
		Attempt:        1,
		MaxAttempts:    3,
		Deadline:       now.Add(time.Hour),
		AvailableAt:    now,
		IdempotencyKey: "key-" + taskID,
		Params:         []byte(`{"batch":10}`),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func openTaskStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func TestTaskMigration18CreatesTasksSchema(t *testing.T) {
	_, path := openTaskStore(t)
	// 通过独立连接读取同一数据库文件验证迁移 18 的表与索引。
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 21 {
		t.Fatalf("schema 版本=%d，期望 21", version)
	}
	var tables, indexes int
	if err := db.QueryRowContext(t.Context(), `
SELECT
  (SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tasks'),
  (SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN
    ('tasks_queue_idx','tasks_lease_idx','tasks_app_lease_idx'))`).Scan(&tables, &indexes); err != nil {
		t.Fatal(err)
	}
	if tables != 1 || indexes != 3 {
		t.Fatalf("tasks 表=%d 索引=%d，期望 1 表 3 索引", tables, indexes)
	}
}

func TestTaskStoreRoundTripAndAppIsolation(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != kerneltask.StatusQueued || value.Attempt != 1 || value.MaxAttempts != 3 ||
		value.Type != "bus.catalog.sync" || value.IdempotencyKey != "key-task-1" ||
		string(value.Params) != `{"batch":10}` || value.ErrorClass != kerneltask.ErrorClassNone ||
		value.LeaseToken != "" || value.LeaseExpiresAt != nil ||
		!value.Deadline.Equal(now.Add(time.Hour)) || !value.AvailableAt.Equal(now) {
		t.Fatalf("任务字段回读不符：%#v", value)
	}
	// App 隔离：其他 App 不可读。
	if _, err := store.GetTask(ctx, "app-b", "task-1"); !errors.Is(err, kerneltask.ErrTaskNotFound) {
		t.Fatalf("跨 App 读取错误=%v，期望 ErrTaskNotFound", err)
	}
	// ListTasks 只返回本 App 的任务。
	if err := store.CreateTask(ctx, newTaskRecord("app-b", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, "app-a", 10)
	if err != nil || len(tasks) != 1 || tasks[0].TaskID != "task-1" {
		t.Fatalf("App A 任务=%#v err=%v", tasks, err)
	}
	// 复合主键：重复创建被拒绝。
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); !errors.Is(err, kerneltask.ErrTaskExists) {
		t.Fatalf("重复创建错误=%v，期望 ErrTaskExists", err)
	}
}

func TestTaskStoreRejectsInvalidTasks(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := newTaskRecord("app-a", "task-1", now)
	cases := map[string]kerneltask.Task{
		"非法状态":       func() kerneltask.Task { value := base; value.Status = kerneltask.StatusRunning; return value }(),
		"非法 attempt": func() kerneltask.Task { value := base; value.Attempt = 0; return value }(),
		"非法幂等键":      func() kerneltask.Task { value := base; value.IdempotencyKey = "非法键"; return value }(),
		"非法参数":       func() kerneltask.Task { value := base; value.Params = []byte("not json"); return value }(),
		"非法任务标识":     func() kerneltask.Task { value := base; value.TaskID = "-bad"; return value }(),
		"deadline 不晚于 available": func() kerneltask.Task {
			value := base
			value.Deadline = now.Add(time.Minute)
			value.AvailableAt = now.Add(2 * time.Minute)
			return value
		}(),
		"携带租约": func() kerneltask.Task {
			value := base
			value.LeaseToken = "lease"
			expires := now.Add(time.Minute)
			value.LeaseExpiresAt = &expires
			return value
		}(),
		"携带错误分类": func() kerneltask.Task {
			value := base
			value.ErrorClass = kerneltask.ErrorClassNonRetryable
			return value
		}(),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if err := store.CreateTask(ctx, value); !errors.Is(err, kerneltask.ErrInvalidTask) {
				t.Fatalf("期望 ErrInvalidTask，实际 err=%v", err)
			}
		})
	}
}

func TestTaskClaimTransitionsAndRenew(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != kerneltask.StatusRunning || claimed.LeaseToken != "lease-1" ||
		claimed.LeaseExpiresAt == nil || !claimed.LeaseExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("领取结果不符：%#v", claimed)
	}
	// 重复领取被拒绝：租约保证同一任务只有一个执行者。
	if _, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-2", now, now.Add(time.Minute)); !errors.Is(err, kerneltask.ErrInvalidTransition) {
		t.Fatalf("重复领取错误=%v，期望 ErrInvalidTransition", err)
	}
	// 租约续期：有效期内成功。
	if err := store.RenewTaskLease(ctx, claimed, now.Add(20*time.Second), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil || stored.LeaseExpiresAt == nil || !stored.LeaseExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("续期结果=%#v err=%v", stored, err)
	}
	// 租约到期后续期失败：ErrLeaseLost。
	renewed := stored
	renewed.LeaseToken = "lease-1"
	if err := store.RenewTaskLease(ctx, renewed, now.Add(3*time.Minute), now.Add(4*time.Minute)); !errors.Is(err, kerneltask.ErrLeaseLost) {
		t.Fatalf("过期续期错误=%v，期望 ErrLeaseLost", err)
	}
	// 错误令牌续期失败。
	wrongToken := claimed
	wrongToken.LeaseToken = "wrong"
	if err := store.RenewTaskLease(ctx, wrongToken, now, now.Add(time.Minute)); !errors.Is(err, kerneltask.ErrLeaseLost) {
		t.Fatalf("错误令牌续期错误=%v，期望 ErrLeaseLost", err)
	}
}

func TestTaskNotDueCannotBeClaimed(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	value := newTaskRecord("app-a", "task-1", now)
	value.AvailableAt = now.Add(10 * time.Minute)
	value.Deadline = now.Add(time.Hour)
	if err := store.CreateTask(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now, now.Add(time.Minute)); !errors.Is(err, kerneltask.ErrInvalidTransition) {
		t.Fatalf("未到期领取错误=%v，期望 ErrInvalidTransition", err)
	}
	// 到期后可领取。
	if _, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now.Add(10*time.Minute), now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestTaskTerminalWritesAreLeaseGuarded(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 错误令牌的完成写入被拒绝（状态仍为 running）。
	wrong := claimed
	wrong.LeaseToken = "wrong"
	if err := store.CompleteTask(ctx, wrong, now.Add(time.Second)); !errors.Is(err, kerneltask.ErrLeaseLost) {
		t.Fatalf("错误令牌完成错误=%v，期望 ErrLeaseLost", err)
	}
	// 正确令牌成功完成。
	if err := store.CompleteTask(ctx, claimed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil || stored.Status != kerneltask.StatusSucceeded || stored.LeaseToken != "" ||
		stored.LeaseExpiresAt != nil || stored.ErrorClass != kerneltask.ErrorClassNone {
		t.Fatalf("完成结果=%#v err=%v", stored, err)
	}
	// 已进入终态后再次完成被拒绝。
	if err := store.CompleteTask(ctx, claimed, now.Add(2*time.Second)); !errors.Is(err, kerneltask.ErrInvalidTransition) {
		t.Fatalf("重复完成错误=%v，期望 ErrInvalidTransition", err)
	}
	// 租约到期后的完成写入失败：副作用未知，租约已丢失。
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-2", now)); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimTask(ctx, "app-a", "task-2", "lease-2", now, now.Add(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := store.CompleteTask(ctx, second, now.Add(200*time.Millisecond)); !errors.Is(err, kerneltask.ErrLeaseLost) {
		t.Fatalf("过期租约完成错误=%v，期望 ErrLeaseLost", err)
	}
}

func TestTaskFailAndRetryTransitions(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailTask(ctx, claimed, kerneltask.ErrorClassDeadline, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil || stored.Status != kerneltask.StatusFailed || stored.ErrorClass != kerneltask.ErrorClassDeadline {
		t.Fatalf("失败结果=%#v err=%v", stored, err)
	}
	// 重试安排：attempt+1、available_at 前进、租约清除。
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-2", now)); err != nil {
		t.Fatal(err)
	}
	claimed2, err := store.ClaimTask(ctx, "app-a", "task-2", "lease-2", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nextAvailable := now.Add(30 * time.Second)
	if err := store.RetryTask(ctx, claimed2, nextAvailable, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored2, err := store.GetTask(ctx, "app-a", "task-2")
	if err != nil || stored2.Status != kerneltask.StatusRetryScheduled || stored2.Attempt != 2 ||
		stored2.LeaseToken != "" || stored2.LeaseExpiresAt != nil || !stored2.AvailableAt.Equal(nextAvailable) {
		t.Fatalf("重试结果=%#v err=%v", stored2, err)
	}
	// 重试后的任务在到期后可以重新领取（可重领）。
	claimed3, err := store.ClaimTask(ctx, "app-a", "task-2", "lease-3", nextAvailable, nextAvailable.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// attempt 已达上限时不允许再安排重试。
	claimed3.Attempt = claimed3.MaxAttempts
	if err := store.RetryTask(ctx, claimed3, nextAvailable.Add(time.Minute), nextAvailable.Add(time.Second)); !errors.Is(err, kerneltask.ErrInvalidTask) {
		t.Fatalf("超限重试错误=%v，期望 ErrInvalidTask", err)
	}
}

func TestTaskCancelQueuedAndRunning(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	cancelled, ok, err := store.CancelQueuedTask(ctx, "app-a", "task-1", now.Add(time.Second))
	if err != nil || !ok || cancelled.Status != kerneltask.StatusCancelled ||
		cancelled.ErrorClass != kerneltask.ErrorClassCancelled {
		t.Fatalf("取消排队任务=%#v ok=%v err=%v", cancelled, ok, err)
	}
	// 已终态任务取消返回 false（调度器据此转向运行中取消）。
	if _, ok, err := store.CancelQueuedTask(ctx, "app-a", "task-1", now.Add(2*time.Second)); err != nil || ok {
		t.Fatalf("重复取消 ok=%v err=%v，期望 ok=false", ok, err)
	}
	// 不存在的任务返回 ErrTaskNotFound。
	if _, _, err := store.CancelQueuedTask(ctx, "app-a", "missing", now.Add(time.Second)); !errors.Is(err, kerneltask.ErrTaskNotFound) {
		t.Fatalf("缺失任务取消错误=%v，期望 ErrTaskNotFound", err)
	}
	// 运行中任务以租约守卫取消。
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-2", now)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTask(ctx, "app-a", "task-2", "lease-2", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelRunningTask(ctx, claimed, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, "app-a", "task-2")
	if err != nil || stored.Status != kerneltask.StatusCancelled || stored.ErrorClass != kerneltask.ErrorClassCancelled {
		t.Fatalf("取消运行中任务=%#v err=%v", stored, err)
	}
	// 错误令牌的取消被拒绝。
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-3", now)); err != nil {
		t.Fatal(err)
	}
	claimed3, err := store.ClaimTask(ctx, "app-a", "task-3", "lease-3", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed3.LeaseToken = "wrong"
	if err := store.CancelRunningTask(ctx, claimed3, now.Add(time.Second)); !errors.Is(err, kerneltask.ErrLeaseLost) {
		t.Fatalf("错误令牌取消错误=%v，期望 ErrLeaseLost", err)
	}
}

func TestTaskDeadTaskRecovery(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-2", now)); err != nil {
		t.Fatal(err)
	}
	// 任务 1 以短租约领取；任务 2 保持排队。
	if _, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-1", now, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if dead, err := store.ListDeadTasks(ctx, now.Add(50*time.Millisecond), 10); err != nil || len(dead) != 0 {
		t.Fatalf("未过期时死亡任务=%#v err=%v", dead, err)
	}
	time.Sleep(150 * time.Millisecond)
	expired := time.Now().UTC()
	dead, err := store.ListDeadTasks(ctx, expired, 10)
	if err != nil || len(dead) != 1 || dead[0].TaskID != "task-1" || dead[0].LeaseToken != "lease-1" {
		t.Fatalf("死亡任务=%#v err=%v", dead, err)
	}
	// 死亡任务按策略重试：attempt+1、租约清除、可重新领取。
	if err := store.RetryDeadTask(ctx, dead[0], expired.Add(time.Minute), expired); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil || stored.Status != kerneltask.StatusRetryScheduled || stored.Attempt != 2 {
		t.Fatalf("死亡任务重试=%#v err=%v", stored, err)
	}
	// 第二个死亡任务按策略确定性终结。
	if _, err := store.ClaimTask(ctx, "app-a", "task-2", "lease-2", expired, expired.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	expired2 := time.Now().UTC()
	dead, err = store.ListDeadTasks(ctx, expired2, 10)
	if err != nil || len(dead) != 1 || dead[0].TaskID != "task-2" {
		t.Fatalf("第二批死亡任务=%#v err=%v", dead, err)
	}
	if err := store.FailDeadTask(ctx, dead[0], kerneltask.ErrorClassLeaseLost, expired2); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetTask(ctx, "app-a", "task-2")
	if err != nil || stored.Status != kerneltask.StatusFailed || stored.ErrorClass != kerneltask.ErrorClassLeaseLost {
		t.Fatalf("死亡任务终结=%#v err=%v", stored, err)
	}
}

// TestTaskStoreCrashRestartRecovery 覆盖 PRD 验收：
// 进程崩溃后重新打开 Store，死亡任务保持持久状态并可确定性恢复/重领。
func TestTaskStoreCrashRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-restart.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-2", now)); err != nil {
		t.Fatal(err)
	}
	// 模拟进程崩溃：任务 1 领取后未终结（租约随后到期），任务 2 仍排队。
	if _, err := store.ClaimTask(ctx, "app-a", "task-1", "lease-crashed", now, now.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// 重启后：死亡任务可被列出并确定性恢复。
	dead, err := reopened.ListDeadTasks(ctx, time.Now().UTC(), 10)
	if err != nil || len(dead) != 1 || dead[0].TaskID != "task-1" || dead[0].LeaseToken != "lease-crashed" {
		t.Fatalf("重启后死亡任务=%#v err=%v", dead, err)
	}
	restartNow := time.Now().UTC()
	if err := reopened.RetryDeadTask(ctx, dead[0], restartNow.Add(time.Minute), restartNow); err != nil {
		t.Fatal(err)
	}
	// 死亡任务重试后可重新领取执行（退避到期后）；排队的任务不受崩溃影响可正常领取。
	claimed, err := reopened.ClaimTask(ctx, "app-a", "task-1", "lease-restarted", restartNow.Add(time.Minute), restartNow.Add(2*time.Minute))
	if err != nil || claimed.Status != kerneltask.StatusRunning || claimed.Attempt != 2 {
		t.Fatalf("重启后重领=%#v err=%v", claimed, err)
	}
	if _, err := reopened.ClaimTask(ctx, "app-a", "task-2", "lease-2", restartNow, restartNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 幂等键与参数在重启后保持完整，供处理器做可重放安全。
	if claimed.IdempotencyKey != "key-task-1" || string(claimed.Params) != `{"batch":10}` {
		t.Fatalf("重启后任务字段丢失：%#v", claimed)
	}
}

// TestTaskClaimIsAtomicUnderConcurrency 覆盖 PRD 验收：
// 并发领取同一任务时只有唯一执行者胜出（租约原子守卫）。
func TestTaskClaimIsAtomicUnderConcurrency(t *testing.T) {
	store, _ := openTaskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateTask(ctx, newTaskRecord("app-a", "task-1", now)); err != nil {
		t.Fatal(err)
	}
	const claimants = 16
	results := make(chan error, claimants)
	var wait sync.WaitGroup
	for index := 0; index < claimants; index++ {
		wait.Add(1)
		go func(sequence int) {
			defer wait.Done()
			_, err := store.ClaimTask(ctx, "app-a", "task-1",
				fmt.Sprintf("lease-%d", sequence), now, now.Add(time.Minute))
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)
	var claimed, rejected int
	for result := range results {
		switch {
		case result == nil:
			claimed++
		case errors.Is(result, kerneltask.ErrInvalidTransition):
			rejected++
		default:
			t.Fatalf("并发领取意外错误=%v", result)
		}
	}
	if claimed != 1 || rejected != claimants-1 {
		t.Fatalf("并发领取 成功=%d 拒绝=%d，期望 成功=1 拒绝=%d", claimed, rejected, claimants-1)
	}
	value, err := store.GetTask(ctx, "app-a", "task-1")
	if err != nil || value.Status != kerneltask.StatusRunning || value.LeaseToken == "" {
		t.Fatalf("领取后状态=%#v err=%v", value, err)
	}
}
