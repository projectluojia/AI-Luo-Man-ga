package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 是测试用可控时钟，调度器通过 config.Now 注入。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// schedulerConfig 返回一份测试用调度器配置：短轮询、长租约（真实时间不会超过
// 假时钟租约），保证测试在秒级完成且不会被租约续期竞态干扰。
func schedulerConfig(clock *fakeClock) Config {
	return Config{
		MaxConcurrent:      4,
		AppCapacity:        2,
		PollInterval:       5 * time.Millisecond,
		LeaseDuration:      5 * time.Minute,
		RetryBaseDelay:     time.Millisecond,
		RetryMaxDelay:      time.Second,
		RecoveryInterval:   20 * time.Millisecond,
		BatchSize:          32,
		DefaultMaxAttempts: 3,
		OutboxCapacity:     64,
		Now:                clock.Now,
	}
}

// waitForCondition 以真实时间轮询等待条件满足。
func waitForCondition(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

func waitForTask(t *testing.T, store Store, appID, taskID string, predicate func(Task) bool, timeout time.Duration) Task {
	t.Helper()
	var found Task
	waitForCondition(t, "任务状态", timeout, func() bool {
		value, err := store.GetTask(context.Background(), appID, taskID)
		if err != nil {
			return false
		}
		found = value
		return predicate(value)
	})
	return found
}

func stopScheduler(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := scheduler.Shutdown(ctx); err != nil {
		t.Fatalf("调度器优雅关闭失败：%v", err)
	}
}

func startScheduler(t *testing.T, scheduler *Scheduler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := scheduler.Start(ctx); err != nil {
		cancel()
		t.Fatalf("调度器启动失败：%v", err)
	}
	return cancel
}

func TestSchedulerExecutesTaskOnce(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-1",
		Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	if stored.Attempt != 1 || stored.ErrorClass != ErrorClassNone ||
		stored.IdempotencyKey != "key-1" || string(stored.Params) != `{"batch":1}` {
		t.Fatalf("成功任务字段不符：%#v", stored)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("执行次数=%d，期望 1", got)
	}
}

func TestSchedulerDelayedTaskRunsAfterAvailableAt(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "delayed", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "delayed",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-delayed",
		Deadline: clock.Now().Add(time.Hour), AvailableAt: clock.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 未到期前必须保持排队。
	time.Sleep(50 * time.Millisecond)
	if stored, err := store.GetTask(context.Background(), value.AppID, value.TaskID); err != nil || stored.Status != StatusQueued {
		t.Fatalf("未到期任务状态=%q err=%v，期望 queued", stored.Status, err)
	}
	// 推进时钟到 available_at 之后（但仍在 deadline 之前），任务必须执行。
	clock.Advance(40 * time.Minute)
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	if got := executions.Load(); got != 1 {
		t.Fatalf("延迟任务执行次数=%d，期望 1", got)
	}
}

func TestSchedulerPeriodicTaskReschedulesItself(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	var scheduler *Scheduler
	if err := registry.Register(TypeSpec{
		TypeID: "periodic", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false,
		Handler: func(ctx context.Context, value Task) error {
			executions.Add(1)
			if executions.Load() >= 3 {
				return nil
			}
			// 周期任务：成功后在处理器内创建下一次执行。
			_, err := scheduler.Create(ctx, CreateRequest{
				AppID: value.AppID, Type: "periodic",
				Params:         append([]byte(nil), value.Params...),
				IdempotencyKey: fmt.Sprintf("periodic-%d", executions.Load()+1),
				MaxAttempts:    1,
				Deadline:       value.Deadline.Add(time.Hour),
			})
			return err
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler = NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "periodic",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "periodic-1",
		MaxAttempts: 1, Deadline: clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "周期任务完成三次执行", 10*time.Second, func() bool {
		tasks, err := store.ListTasks(context.Background(), "campus-services", 10)
		if err != nil || len(tasks) < 3 {
			return false
		}
		for _, item := range tasks {
			if item.Status != StatusSucceeded {
				return false
			}
		}
		return true
	})
	if got := executions.Load(); got != 3 {
		t.Fatalf("周期任务执行次数=%d，期望 3", got)
	}
}

// TestSchedulerDuplicateClaimDoesNotDuplicateSideEffect 覆盖 PRD 验收：
// 两个调度器共享同一持久 Store 并发领取同一任务，副作用只能发生一次。
func TestSchedulerDuplicateClaimDoesNotDuplicateSideEffect(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	config := schedulerConfig(clock)
	first := NewScheduler(store, registry, config)
	second := NewScheduler(store, registry, config)
	cancelFirst := startScheduler(t, first)
	defer cancelFirst()
	cancelSecond := startScheduler(t, second)
	defer cancelSecond()
	defer stopScheduler(t, first)
	defer stopScheduler(t, second)
	value, err := first.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-1",
		Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	// 留出第二个调度器重复领取的窗口。
	time.Sleep(100 * time.Millisecond)
	if got := executions.Load(); got != 1 {
		t.Fatalf("重复领取导致副作用执行 %d 次，期望 1 次", got)
	}
}

// TestSchedulerRecoversCrashedTaskDeterministically 覆盖 PRD 验收：
// 进程崩溃（领取后未续租、未终结）后，任务由持久状态确定性恢复。
func TestSchedulerRecoversCrashedTaskDeterministically(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	value := newTestTaskAt(clock.Now().UTC())
	if err := store.CreateTask(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	// 模拟进程领取后崩溃：直接以短租约领取，随后不再续租。
	if _, err := store.ClaimTask(context.Background(), value.AppID, value.TaskID,
		"crashed-lease", clock.Now(), clock.Now().Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	// 调度器启动时先执行恢复循环，确定性处理租约过期的运行中任务。
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	recovered := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusRetryScheduled && item.Attempt == 2
	}, 5*time.Second)
	if recovered.ErrorClass != ErrorClassNone {
		t.Fatalf("重试安排不应携带错误分类，实际=%q", recovered.ErrorClass)
	}
	// 退避到期后重新领取执行，且不重复副作用。
	clock.Advance(time.Second)
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	if got := executions.Load(); got != 1 {
		t.Fatalf("崩溃恢复后执行次数=%d，期望 1", got)
	}
}

// TestSchedulerCrashedTaskWithUnsafeTypeIsFailed 覆盖 PRD 验收：
// 不允许重试的类型，其崩溃任务按租约丢失确定性终结，不自动重试。
func TestSchedulerCrashedTaskWithUnsafeTypeIsFailed(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	value := newTestTaskAt(clock.Now().UTC())
	if err := store.CreateTask(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	// 模拟进程领取后崩溃：直接以短租约领取，随后不再续租。
	if _, err := store.ClaimTask(context.Background(), value.AppID, value.TaskID,
		"crashed-lease", clock.Now(), clock.Now().Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	// 调度器启动时先执行恢复循环，确定性终结租约过期的运行中任务。
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	recovered := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if recovered.ErrorClass != ErrorClassLeaseLost || recovered.Attempt != 1 {
		t.Fatalf("不安全类型崩溃任务=%#v，期望 lease_lost 失败且不重试", recovered)
	}
	if got := executions.Load(); got != 0 {
		t.Fatalf("不安全类型任务不应执行，实际执行 %d 次", got)
	}
}

// TestSchedulerUnsafeFailureIsNotAutoRetried 覆盖 PRD 验收：
// 不安全副作用默认不自动重试；未显式标记可重试的错误一律失败关闭。
func TestSchedulerUnsafeFailureIsNotAutoRetried(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "unsafe", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false,
		Handler: func(context.Context, Task) error {
			executions.Add(1)
			return errors.New("不可安全重试的副作用失败")
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "unsafe",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-unsafe",
		MaxAttempts: 3, Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if failed.ErrorClass != ErrorClassNonRetryable || failed.Attempt != 1 {
		t.Fatalf("不安全失败任务=%#v，期望 non_retryable 且不重试", failed)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("执行次数=%d，期望 1", got)
	}
}

// TestSchedulerRetryableFailureIsBoundedAndBackedOff 覆盖 PRD 验收：
// 显式可重试错误按类型策略自动重试，重试带退避且有次数上限。
func TestSchedulerRetryableFailureIsBoundedAndBackedOff(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "flaky", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true,
		Handler: func(context.Context, Task) error {
			executions.Add(1)
			return Retryable(errors.New("transient"))
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "flaky",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-flaky",
		MaxAttempts: 3, Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusRetryScheduled && item.Attempt == 2
	}, 5*time.Second)
	clock.Advance(time.Second)
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusRetryScheduled && item.Attempt == 3
	}, 5*time.Second)
	clock.Advance(time.Second)
	failed := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if failed.Attempt != 3 || failed.ErrorClass != ErrorClassRetryable {
		t.Fatalf("有界重试任务=%#v，期望 attempt=3 error_class=retryable", failed)
	}
	if got := executions.Load(); got != 3 {
		t.Fatalf("重试执行次数=%d，期望 3", got)
	}
}

// TestSchedulerDeadlineTimeoutIsTerminalFailure 覆盖 PRD 验收：
// 任务有 deadline；执行超时副作用未知，按 deadline_exceeded 确定性终结且不自动重试。
func TestSchedulerDeadlineTimeoutIsTerminalFailure(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	// 即使类型允许重试，超时也不得自动重试。
	if err := registry.Register(TypeSpec{
		TypeID: "blocking", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true,
		Handler: func(ctx context.Context, _ Task) error {
			executions.Add(1)
			<-ctx.Done()
			return ctx.Err()
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "blocking",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-deadline",
		MaxAttempts: 3, Deadline: clock.Now().Add(150 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if failed.ErrorClass != ErrorClassDeadline || failed.Attempt != 1 {
		t.Fatalf("超时任务=%#v，期望 deadline_exceeded 且 attempt=1", failed)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("执行次数=%d，期望 1", got)
	}
}

// TestSchedulerAppCapacityIsolatesCongestedApp 覆盖 PRD 验收：
// 某 App 的任务拥塞不能占满整个 Deployment；App 容量上限保证其他 App 继续执行。
func TestSchedulerAppCapacityIsolatesCongestedApp(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	config := schedulerConfig(clock)
	config.MaxConcurrent = 2
	config.AppCapacity = 1

	releaseA := make(chan struct{})
	var aExecutions, bExecutions atomic.Int32
	var aConcurrent atomic.Int32
	var aMaxConcurrent atomic.Int32
	handlerA := func(ctx context.Context, _ Task) error {
		current := aConcurrent.Add(1)
		defer aConcurrent.Add(-1)
		for {
			maximum := aMaxConcurrent.Load()
			if current <= maximum || aMaxConcurrent.CompareAndSwap(maximum, current) {
				break
			}
		}
		aExecutions.Add(1)
		select {
		case <-releaseA:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	handlerB := func(context.Context, Task) error {
		bExecutions.Add(1)
		return nil
	}
	if err := registry.Register(TypeSpec{
		TypeID: "slow.a", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: handlerA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(TypeSpec{
		TypeID: "quick.b", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: handlerB,
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, config)
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)

	// App A 先占用其唯一容量槽位。
	a1, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "app-a", Type: "slow.a", Params: []byte(`{"batch":1}`),
		IdempotencyKey: "a-1", Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, a1.AppID, a1.TaskID, func(item Task) bool {
		return item.Status == StatusRunning
	}, 5*time.Second)
	// App A 再次排队一个任务，App B 也排队一个任务。
	a2, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "app-a", Type: "slow.a", Params: []byte(`{"batch":2}`),
		IdempotencyKey: "a-2", Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	b1, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "app-b", Type: "quick.b", Params: []byte(`{"batch":1}`),
		IdempotencyKey: "b-1", Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// App B 的任务必须无视 App A 的拥塞完成执行。
	waitForTask(t, store, b1.AppID, b1.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	// App A 的第二个任务因 App 容量已满必须保持排队。
	a2Stored, err := store.GetTask(context.Background(), a2.AppID, a2.TaskID)
	if err != nil || a2Stored.Status != StatusQueued {
		t.Fatalf("拥塞 App 的排队任务状态=%q err=%v，期望 queued", a2Stored.Status, err)
	}
	close(releaseA)
	waitForTask(t, store, a1.AppID, a1.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	waitForTask(t, store, a2.AppID, a2.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	if got := aMaxConcurrent.Load(); got > 1 {
		t.Fatalf("App A 最大并发执行数=%d，App 容量上限必须保证 <= 1", got)
	}
	if got := aExecutions.Load(); got != 2 {
		t.Fatalf("App A 执行次数=%d，期望 2", got)
	}
	if got := bExecutions.Load(); got != 1 {
		t.Fatalf("App B 执行次数=%d，期望 1", got)
	}
}

func TestSchedulerCreateRejectsUnknownTypeAndInvalidParams(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	now := clock.Now()
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "unknown.type",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-1", Deadline: now.Add(time.Hour),
	}); !errors.Is(err, ErrTaskTypeUnknown) {
		t.Fatalf("未注册类型错误=%v，期望 ErrTaskTypeUnknown", err)
	}
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"unknown":true}`), IdempotencyKey: "key-1", Deadline: now.Add(time.Hour),
	}); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("非法参数错误=%v，期望 ErrInvalidParams", err)
	}
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-1", Deadline: now.Add(-time.Minute),
	}); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("过期 deadline 错误=%v，期望 ErrInvalidTask", err)
	}
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-1",
		Deadline: now.Add(time.Hour), AvailableAt: now.Add(-time.Minute),
	}); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("过去 available_at 错误=%v，期望 ErrInvalidTask", err)
	}
	if _, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "非法键", Deadline: now.Add(time.Hour),
	}); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("非法幂等键错误=%v，期望 ErrInvalidTask", err)
	}
}

// TestSchedulerNeverExecutesUnregisteredType 覆盖 PRD 验收：
// 调度器不执行任意任务名；未注册类型的持久化任务被确定性拒绝。
func TestSchedulerNeverExecutesUnregisteredType(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	// 直接向持久存储注入一个未注册类型的任务，绕过创建校验。
	value := newTestTaskAt(clock.Now().UTC())
	value.Type = "evil.type"
	value.IdempotencyKey = "key-evil"
	if err := store.CreateTask(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	failed := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if failed.ErrorClass != ErrorClassNonRetryable || failed.Attempt != 1 {
		t.Fatalf("未注册类型任务=%#v，期望 non_retryable 拒绝执行", failed)
	}
	if got := executions.Load(); got != 0 {
		t.Fatalf("未注册类型处理器被执行 %d 次", got)
	}
}

// TestSchedulerRejectsInvalidStoredParams 覆盖 PRD 验收：
// 执行前按注册 Schema 二次校验参数，非法参数的任务被确定性拒绝。
func TestSchedulerRejectsInvalidStoredParams(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { executions.Add(1); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	value := newTestTaskAt(clock.Now().UTC())
	value.Params = []byte(`{"unknown":true}`)
	if err := store.CreateTask(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	failed := waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusFailed
	}, 5*time.Second)
	if failed.ErrorClass != ErrorClassNonRetryable {
		t.Fatalf("非法参数任务=%#v，期望 non_retryable 拒绝执行", failed)
	}
	if got := executions.Load(); got != 0 {
		t.Fatalf("非法参数处理器被执行 %d 次", got)
	}
}

// TestSchedulerGracefulShutdownPersistsRunningTask 覆盖 PRD 验收：
// 优雅关闭在排空超时后强制取消执行；运行中任务保持 running 持久状态，
// 下次启动由恢复流程确定性处理。
func TestSchedulerGracefulShutdownPersistsRunningTask(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	release := make(chan struct{})
	var executions atomic.Int32
	if err := registry.Register(TypeSpec{
		TypeID: "blocking", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true,
		Handler: func(ctx context.Context, _ Task) error {
			executions.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "blocking",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-blocking",
		MaxAttempts: 3, Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusRunning
	}, 5*time.Second)
	// 排空超时：处理器被强制取消，任务保持 running 等待恢复。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	shutdownErr := scheduler.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr == nil {
		t.Fatal("期望排空超时错误，但调度器成功关闭了阻塞任务")
	}
	stored, err := store.GetTask(context.Background(), value.AppID, value.TaskID)
	if err != nil || stored.Status != StatusRunning {
		t.Fatalf("关闭后任务状态=%q err=%v，期望保持 running", stored.Status, err)
	}
	// 模拟进程重启：租约到期后由新调度器确定性恢复并再次执行。
	close(release)
	clock.Advance(10 * time.Minute)
	restarted := NewScheduler(store, registry, schedulerConfig(clock))
	restartCancel := startScheduler(t, restarted)
	defer restartCancel()
	defer stopScheduler(t, restarted)
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusRetryScheduled
	}, 5*time.Second)
	clock.Advance(time.Second)
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	if got := executions.Load(); got != 2 {
		t.Fatalf("重启恢复后执行次数=%d，期望 2（崩溃前 1 次 + 恢复后 1 次）", got)
	}
}

func TestSchedulerCancelsQueuedAndRunningTasks(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	var executions atomic.Int32
	release := make(chan struct{})
	if err := registry.Register(TypeSpec{
		TypeID: "blocking", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false,
		Handler: func(ctx context.Context, _ Task) error {
			executions.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, registry, schedulerConfig(clock))
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	// 取消排队中的任务：不得执行。
	queued, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "blocking",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-queued",
		Deadline: clock.Now().Add(2 * time.Hour), AvailableAt: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := scheduler.Cancel(context.Background(), queued.AppID, queued.TaskID)
	if err != nil || !cancelled {
		t.Fatalf("取消排队任务 cancelled=%v err=%v", cancelled, err)
	}
	queuedStored, err := store.GetTask(context.Background(), queued.AppID, queued.TaskID)
	if err != nil || queuedStored.Status != StatusCancelled {
		t.Fatalf("排队任务取消后状态=%q err=%v", queuedStored.Status, err)
	}
	// 取消运行中的任务：取消传播到执行上下文并持久化 cancelled。
	running, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "blocking",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-running",
		Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, running.AppID, running.TaskID, func(item Task) bool {
		return item.Status == StatusRunning
	}, 5*time.Second)
	cancelled, err = scheduler.Cancel(context.Background(), running.AppID, running.TaskID)
	if err != nil || !cancelled {
		t.Fatalf("取消运行中任务 cancelled=%v err=%v", cancelled, err)
	}
	waitForTask(t, store, running.AppID, running.TaskID, func(item Task) bool {
		return item.Status == StatusCancelled
	}, 5*time.Second)
	if got := executions.Load(); got != 1 {
		t.Fatalf("运行中任务执行次数=%d，期望 1", got)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) Publish(_ context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingSink) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// TestSchedulerOutboxDeliversLifecycleEvents 覆盖 Outbox 事件投递职责：
// 事件按生命周期顺序投递，只携带稳定标识与闭式状态，不携带参数正文。
func TestSchedulerOutboxDeliversLifecycleEvents(t *testing.T) {
	store := newMemStore()
	clock := newFakeClock()
	registry := NewTypeRegistry()
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: false, Handler: func(context.Context, Task) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	config := schedulerConfig(clock)
	config.EventSink = sink
	scheduler := NewScheduler(store, registry, config)
	startCancel := startScheduler(t, scheduler)
	defer startCancel()
	defer stopScheduler(t, scheduler)
	value, err := scheduler.Create(context.Background(), CreateRequest{
		AppID: "campus-services", Type: "bus.catalog.sync",
		Params: []byte(`{"batch":1}`), IdempotencyKey: "key-outbox",
		Deadline: clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTask(t, store, value.AppID, value.TaskID, func(item Task) bool {
		return item.Status == StatusSucceeded
	}, 5*time.Second)
	var events []Event
	waitForCondition(t, "Outbox 事件投递", 5*time.Second, func() bool {
		events = sink.all()
		return len(events) >= 3
	})
	var types []EventType
	for _, event := range events {
		types = append(types, event.Type)
		if event.AppID != "campus-services" || event.TaskID != value.TaskID ||
			event.IdempotencyKey != "key-outbox" {
			t.Fatalf("事件携带非法标识：%#v", event)
		}
		if event.Type == EventCreated && event.Status != StatusQueued {
			t.Fatalf("创建事件状态=%q", event.Status)
		}
		if event.Type == EventClaimed && event.Status != StatusRunning {
			t.Fatalf("领取事件状态=%q", event.Status)
		}
		if event.Type == EventSucceeded && (event.Status != StatusSucceeded || event.ErrorClass != ErrorClassNone) {
			t.Fatalf("成功事件=%#v", event)
		}
	}
	want := []EventType{EventCreated, EventClaimed, EventSucceeded}
	for index, eventType := range want {
		if types[index] != eventType {
			t.Fatalf("事件顺序=%v，期望 %v", types, want)
		}
	}
}
