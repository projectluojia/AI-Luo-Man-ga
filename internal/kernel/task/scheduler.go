package task

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// 调度器默认配置；生产接线可按需覆盖。
const (
	defaultMaxConcurrent    = 16 // 全局并发 worker 上限
	defaultAppCapacity      = 4  // 单个 App 并发任务上限（防拥塞）
	defaultPollInterval     = 500 * time.Millisecond
	defaultLeaseDuration    = 15 * time.Second
	defaultRetryBaseDelay   = 500 * time.Millisecond
	defaultRetryMaxDelay    = 30 * time.Second
	defaultRecoveryInterval = 5 * time.Second
	defaultBatchSize        = 32
	defaultMaxAttempts      = 3
	terminalWriteTimeout    = 5 * time.Second // 终态写入的脱离式超时
)

// Config 是调度器配置。零值会由 NewScheduler 填入默认值。
type Config struct {
	MaxConcurrent      int           // 全局并发 worker 上限
	AppCapacity        int           // 单个 App 并发任务上限
	PollInterval       time.Duration // 轮询到期任务间隔
	LeaseDuration      time.Duration // 领取租约时长
	RetryBaseDelay     time.Duration // 重试退避基数
	RetryMaxDelay      time.Duration // 重试退避上限
	RecoveryInterval   time.Duration // 死亡任务恢复间隔
	BatchSize          int           // 每次轮询/恢复的批量上限
	DefaultMaxAttempts uint32        // 创建任务未指定 MaxAttempts 时的默认值
	Now                func() time.Time
}

// execution 记录一次正在运行的领取，用于取消传播。
// cancel 字段由 execute 协程写入、Cancel 方法读取，使用互斥锁同步；
// cancelled 标志先于 cancel 读取被设置，保证取消不会因时序丢失。
type execution struct {
	appID     string
	taskID    string
	mu        sync.Mutex
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

type appSlots struct {
	ch chan struct{}
}

// Scheduler 轮询领取到期任务、续租、恢复死亡任务，并以有界并发执行。
// 它只依赖 Store 端口与 TypeRegistry，不直接依赖具体数据库。
type Scheduler struct {
	store    Store
	registry *TypeRegistry
	config   Config
	now      func() time.Time

	mu          sync.Mutex
	started     bool
	runCtx      context.Context
	stopRun     context.CancelFunc
	workerCtx   context.Context
	stopWorkers context.CancelFunc
	workerWG    sync.WaitGroup
	executions  map[string]*execution
	workers     chan struct{}
	appSlots    map[string]*appSlots
}

// NewScheduler 创建调度器并应用默认配置。
func NewScheduler(store Store, registry *TypeRegistry, config Config) *Scheduler {
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = defaultMaxConcurrent
	}
	if config.AppCapacity <= 0 {
		config.AppCapacity = defaultAppCapacity
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = defaultLeaseDuration
	}
	if config.RetryBaseDelay <= 0 {
		config.RetryBaseDelay = defaultRetryBaseDelay
	}
	if config.RetryMaxDelay <= 0 {
		config.RetryMaxDelay = defaultRetryMaxDelay
	}
	if config.RetryMaxDelay < config.RetryBaseDelay {
		config.RetryMaxDelay = config.RetryBaseDelay
	}
	if config.RecoveryInterval <= 0 {
		config.RecoveryInterval = defaultRecoveryInterval
	}
	if config.BatchSize <= 0 {
		config.BatchSize = defaultBatchSize
	}
	if config.BatchSize > 1000 {
		config.BatchSize = 1000
	}
	if config.DefaultMaxAttempts == 0 {
		config.DefaultMaxAttempts = defaultMaxAttempts
	}
	if config.DefaultMaxAttempts > maxAttempts {
		config.DefaultMaxAttempts = maxAttempts
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Scheduler{
		store:    store,
		registry: registry,
		config:   config,
		now:      now,
	}
}

// Start 启动轮询与死亡任务恢复循环。
// ctx 取消后轮询停止接活；运行中任务由 Shutdown 决定排空或持久化。
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true
	s.workerCtx, s.stopWorkers = context.WithCancel(context.WithoutCancel(ctx))
	s.runCtx, s.stopRun = context.WithCancel(ctx)
	s.workers = make(chan struct{}, s.config.MaxConcurrent)
	s.appSlots = make(map[string]*appSlots)
	s.executions = make(map[string]*execution)
	s.workerWG.Add(1)
	go s.pollLoop()
	observe.Info(s.runCtx, "后台任务调度器已启动",
		observe.IntAttr("max_concurrent", s.config.MaxConcurrent),
		observe.IntAttr("app_capacity", s.config.AppCapacity),
		observe.Int64Attr("poll_interval_ms", s.config.PollInterval.Milliseconds()),
	)
	return nil
}

// Shutdown 优雅关闭：先停止接活，再有界排空运行中任务；排空超时则强制取消
// 运行中执行，任务保持 running 持久化状态，由下次启动的恢复流程确定性处理。
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	s.mu.Unlock()

	s.stopRun()
	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		observe.Info(context.Background(), "后台任务调度器已安全关闭")
		return nil
	case <-ctx.Done():
		s.stopWorkers()
		waitDone := make(chan struct{})
		go func() {
			s.workerWG.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-ctx.Done():
		}
		return fmt.Errorf("等待后台任务排空超时：%w", ctx.Err())
	}
}

// pollLoop 是调度器主循环：启动时先恢复死亡任务并立即轮询一次，随后按
// 固定间隔轮询到期任务，并按恢复间隔扫描租约过期的死亡任务。
func (s *Scheduler) pollLoop() {
	defer s.workerWG.Done()
	pollTicker := time.NewTicker(s.config.PollInterval)
	recoveryTicker := time.NewTicker(s.config.RecoveryInterval)
	defer pollTicker.Stop()
	defer recoveryTicker.Stop()
	s.recoverDeadTasks()
	s.poll()
	for {
		select {
		case <-s.runCtx.Done():
			return
		case <-pollTicker.C:
			s.poll()
		case <-recoveryTicker.C:
			s.recoverDeadTasks()
		}
	}
}

// poll 领取一批到期任务并分发；领取前先获取全局与 App 两级槽位，
// 避免单个 App 的任务拥塞占满整个 Deployment。
func (s *Scheduler) poll() {
	if s.runCtx.Err() != nil {
		return
	}
	now := s.now().UTC()
	due, err := s.store.ListDueTasks(s.runCtx, now, s.config.BatchSize)
	if err != nil {
		if s.runCtx.Err() == nil {
			observe.Error(s.runCtx, "读取到期后台任务失败", err)
		}
		return
	}
	for _, candidate := range due {
		if !s.acquireSlots(candidate.AppID) {
			continue
		}
		leaseToken := uuid.NewString()
		claimed, claimErr := s.store.ClaimTask(s.runCtx, candidate.AppID, candidate.TaskID, leaseToken, now, now.Add(s.config.LeaseDuration))
		if claimErr != nil {
			s.releaseSlots(candidate.AppID)
			if !errors.Is(claimErr, ErrInvalidTransition) && !errors.Is(claimErr, ErrLeaseLost) && s.runCtx.Err() == nil {
				observe.Error(s.runCtx, "领取后台任务失败", claimErr,
					observe.StringAttr("app_id", candidate.AppID),
					observe.StringAttr("task_id", candidate.TaskID),
				)
			}
			continue
		}
		s.dispatch(claimed, now)
	}
}

// dispatch 检查任务 deadline 后启动执行 goroutine。已过 deadline 的任务
// 不执行，直接持久化超时终态（确定性结果）。
func (s *Scheduler) dispatch(value Task, claimedAt time.Time) {
	if !value.Deadline.After(claimedAt) {
		observe.Warn(s.runCtx, "领取时任务 deadline 已过期，直接终结",
			observe.StringAttr("app_id", value.AppID),
			observe.StringAttr("task_id", value.TaskID),
			observe.StringAttr("task_type", value.Type),
		)
		s.transitionFailure(value, ErrorClassDeadline)
		s.releaseSlots(value.AppID)
		return
	}
	exec := &execution{appID: value.AppID, taskID: value.TaskID}
	s.mu.Lock()
	s.executions[executionKey(value.AppID, value.TaskID)] = exec
	s.mu.Unlock()
	s.workerWG.Add(1)
	go s.execute(value, exec)
}

func (s *Scheduler) execute(value Task, exec *execution) {
	defer s.workerWG.Done()
	defer s.releaseSlots(value.AppID)

	base := context.WithoutCancel(s.workerCtx)
	base = observe.With(base,
		observe.Component("task_scheduler"),
		observe.StringAttr("app_id", value.AppID),
		observe.StringAttr("task_id", value.TaskID),
		observe.StringAttr("task_type", value.Type),
		observe.IntAttr("attempt", int(value.Attempt)),
	)
	execCtx, cancel := context.WithDeadline(base, value.Deadline)
	exec.mu.Lock()
	exec.cancel = cancel
	exec.mu.Unlock()
	defer cancel()
	defer func() {
		s.mu.Lock()
		delete(s.executions, executionKey(value.AppID, value.TaskID))
		s.mu.Unlock()
	}()

	// 封闭类型与参数双重校验：调度器绝不执行未注册类型或非法参数的任务。
	spec, ok := s.registry.Lookup(value.Type)
	if !ok {
		observe.Error(execCtx, "任务类型已从注册表移除，拒绝执行", ErrTaskTypeUnavailable)
		s.transitionFailure(value, ErrorClassNonRetryable)
		return
	}
	if err := s.registry.ValidateParams(value.Type, value.Params); err != nil {
		observe.Error(execCtx, "任务参数未通过注册 Schema 校验，拒绝执行", err)
		s.transitionFailure(value, ErrorClassNonRetryable)
		return
	}

	// 领取后、执行前已被显式取消。
	if exec.cancelled.Load() {
		s.transitionCancelled(value)
		return
	}

	leaseCtx, stopLease := context.WithCancel(execCtx)
	leaseFailure := make(chan error, 1)
	go s.renewLease(leaseCtx, cancel, value, leaseFailure)
	defer stopLease()

	observe.Info(execCtx, "开始执行后台任务",
		observe.Int64Attr("deadline_unix_ms", value.Deadline.UnixMilli()),
		observe.Int64Attr("available_unix_ms", value.AvailableAt.UnixMilli()),
		observe.IntAttr("max_attempts", int(value.MaxAttempts)),
	)
	resultErr := spec.Handler(execCtx, value)
	s.finishExecution(execCtx, value, exec, resultErr, leaseFailure)
}

// finishExecution 根据执行上下文与错误分类确定终态或重试安排。
//   - deadline 到期：副作用未知，直接失败（不可自动重试）；
//   - 显式取消：持久化 cancelled；
//   - 租约丢失：按类型策略决定自动重试或失败；
//   - 部署关闭强制取消：保持 running 持久化状态，由下次启动恢复；
//   - 其余情况按错误分类处理，重试受类型 AllowRetry 与次数上限约束。
func (s *Scheduler) finishExecution(execCtx context.Context, value Task, exec *execution, resultErr error, leaseFailure <-chan error) {
	switch {
	case errors.Is(execCtx.Err(), context.DeadlineExceeded):
		observe.Error(execCtx, "后台任务执行超过 deadline", context.DeadlineExceeded)
		s.transitionFailure(value, ErrorClassDeadline)
		return
	case errors.Is(execCtx.Err(), context.Canceled) && exec.cancelled.Load():
		observe.Warn(execCtx, "后台任务已被显式取消")
		s.transitionCancelled(value)
		return
	case errors.Is(execCtx.Err(), context.Canceled):
		select {
		case renewalErr := <-leaseFailure:
			observe.Error(execCtx, "后台任务租约续期失败", renewalErr)
			s.transitionLeaseLost(value)
		default:
			observe.Warn(execCtx, "调度器关闭，后台任务保持运行状态等待下次启动恢复")
		}
		return
	}
	if resultErr == nil {
		s.transitionSucceeded(value)
		return
	}
	switch ClassifyFailure(resultErr) {
	case ErrorClassDeadline:
		observe.Error(execCtx, "后台任务报告超时", resultErr)
		s.transitionFailure(value, ErrorClassDeadline)
	case ErrorClassRetryable:
		if s.retryAllowed(value, ErrorClassRetryable) {
			observe.Error(execCtx, "后台任务失败，已安排退避重试", resultErr)
			s.transitionRetry(value, ErrorClassRetryable)
		} else {
			observe.Error(execCtx, "后台任务失败且不允许自动重试", resultErr)
			s.transitionFailure(value, ErrorClassRetryable)
		}
	default:
		observe.Error(execCtx, "后台任务执行失败", resultErr)
		s.transitionFailure(value, ClassifyFailure(resultErr))
	}
}

// retryAllowed 决定是否允许自动重试：错误必须可重试，类型必须显式允许，
// 且当前尝试次数未达上限。
func (s *Scheduler) retryAllowed(value Task, errorClass ErrorClass) bool {
	if errorClass != ErrorClassRetryable && errorClass != ErrorClassLeaseLost {
		return false
	}
	spec, ok := s.registry.Lookup(value.Type)
	if !ok || !spec.AllowRetry {
		return false
	}
	return value.Attempt < value.MaxAttempts
}

// renewLease 周期续租；续期失败时取消执行上下文并上报失败原因。
func (s *Scheduler) renewLease(ctx context.Context, cancel context.CancelFunc, value Task, failure chan<- error) {
	interval := s.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case renewedAt := <-ticker.C:
			renewContext, renewCancel := context.WithTimeout(ctx, interval)
			err := s.store.RenewTaskLease(renewContext, value, renewedAt.UTC(), renewedAt.UTC().Add(s.config.LeaseDuration))
			renewCancel()
			if err != nil {
				select {
				case failure <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}

// recoverDeadTasks 确定性恢复租约已到期但未终结的运行中任务：
// 允许重试的类型安排退避重试，其余按策略终结；恢复同样有界分批。
func (s *Scheduler) recoverDeadTasks() {
	if s.runCtx.Err() != nil {
		return
	}
	now := s.now().UTC()
	dead, err := s.store.ListDeadTasks(s.runCtx, now, s.config.BatchSize)
	if err != nil {
		if s.runCtx.Err() == nil {
			observe.Error(s.runCtx, "读取租约过期的后台任务失败", err)
		}
		return
	}
	for _, value := range dead {
		ctx, cancel := s.detached(s.runCtx)
		if !value.Deadline.After(now) {
			failErr := s.store.FailDeadTask(ctx, value, ErrorClassDeadline, now)
			cancel()
			s.logRecoveryOutcome(ctx, value, failErr, StatusFailed, ErrorClassDeadline)
			continue
		}
		if s.retryAllowed(value, ErrorClassLeaseLost) {
			nextAvailableAt := now.Add(s.retryDelay(value))
			if nextAvailableAt.After(value.Deadline) || nextAvailableAt.Equal(value.Deadline) {
				failErr := s.store.FailDeadTask(ctx, value, ErrorClassLeaseLost, now)
				cancel()
				s.logRecoveryOutcome(ctx, value, failErr, StatusFailed, ErrorClassLeaseLost)
				continue
			}
			retryErr := s.store.RetryDeadTask(ctx, value, nextAvailableAt, now)
			cancel()
			if retryErr == nil {
				observe.Warn(ctx, "租约过期的后台任务已安排重新执行",
					observe.StringAttr("app_id", value.AppID),
					observe.StringAttr("task_id", value.TaskID),
					observe.IntAttr("next_attempt", int(value.Attempt+1)),
				)
			} else if s.runCtx.Err() == nil {
				s.logRecoveryOutcome(ctx, value, retryErr, StatusRetryScheduled, ErrorClassLeaseLost)
			}
		} else {
			failErr := s.store.FailDeadTask(ctx, value, ErrorClassLeaseLost, now)
			cancel()
			s.logRecoveryOutcome(ctx, value, failErr, StatusFailed, ErrorClassLeaseLost)
		}
	}
}

func (s *Scheduler) logRecoveryOutcome(ctx context.Context, value Task, transitionErr error, status string, errorClass ErrorClass) {
	if transitionErr == nil {
		observe.Warn(ctx, "租约过期的后台任务已确定性终结",
			observe.StringAttr("app_id", value.AppID),
			observe.StringAttr("task_id", value.TaskID),
			observe.StringAttr("status", status),
			observe.StringAttr("error_class", string(errorClass)),
		)
		return
	}
	if !errors.Is(transitionErr, ErrInvalidTransition) && !errors.Is(transitionErr, ErrLeaseLost) {
		observe.Error(ctx, "恢复死亡后台任务失败", transitionErr,
			observe.StringAttr("app_id", value.AppID),
			observe.StringAttr("task_id", value.TaskID),
		)
	}
}

// Create 校验类型与参数 Schema 后持久化一个新任务。
// 支持定时（AvailableAt）、延迟（AvailableAt=未来时间）与周期任务
// （处理器在成功/失败后通过 Create 创建下一次执行）三种形态。
func (s *Scheduler) Create(ctx context.Context, request CreateRequest) (Task, error) {
	if _, ok := s.registry.Lookup(request.Type); !ok {
		return Task{}, ErrTaskTypeUnknown
	}
	if err := s.registry.ValidateParams(request.Type, request.Params); err != nil {
		return Task{}, err
	}
	now := s.now().UTC()
	if request.Deadline.IsZero() || !request.Deadline.After(now) {
		return Task{}, fmt.Errorf("%w: deadline 必须晚于当前时间", ErrInvalidTask)
	}
	availableAt := request.AvailableAt
	if availableAt.IsZero() {
		availableAt = now
	}
	if availableAt.Before(now) {
		return Task{}, fmt.Errorf("%w: available_at 不能早于当前时间", ErrInvalidTask)
	}
	if !request.Deadline.After(availableAt) {
		return Task{}, fmt.Errorf("%w: available_at 必须早于 deadline", ErrInvalidTask)
	}
	attemptLimit := request.MaxAttempts
	if attemptLimit == 0 {
		attemptLimit = s.config.DefaultMaxAttempts
	}
	if attemptLimit < 1 || attemptLimit > maxAttempts {
		return Task{}, fmt.Errorf("%w: max_attempts 超出允许范围", ErrInvalidTask)
	}
	taskID := request.TaskID
	if taskID == "" {
		taskID = uuid.NewString()
	}
	value := Task{
		AppID:          request.AppID,
		TaskID:         taskID,
		Type:           request.Type,
		Status:         StatusQueued,
		Attempt:        1,
		MaxAttempts:    attemptLimit,
		Deadline:       request.Deadline.UTC(),
		AvailableAt:    availableAt.UTC(),
		IdempotencyKey: request.IdempotencyKey,
		Params:         append([]byte(nil), request.Params...),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := ValidateNewTask(value); err != nil {
		return Task{}, err
	}
	if err := s.store.CreateTask(ctx, value); err != nil {
		return Task{}, err
	}
	observe.Info(ctx, "后台任务已创建",
		observe.StringAttr("app_id", value.AppID),
		observe.StringAttr("task_id", value.TaskID),
		observe.StringAttr("task_type", value.Type),
		observe.Int64Attr("deadline_unix_ms", value.Deadline.UnixMilli()),
	)
	return value, nil
}

// Cancel 取消排队、等待重试或运行中的任务。取消运行中任务会向执行者
// 传播取消；终态写入由执行者的租约守卫保证原子性。
func (s *Scheduler) Cancel(ctx context.Context, appID, taskID string) (bool, error) {
	_, cancelled, err := s.store.CancelQueuedTask(ctx, appID, taskID, s.now().UTC())
	if err != nil {
		return false, err
	}
	if cancelled {
		return true, nil
	}
	s.mu.Lock()
	exec := s.executions[executionKey(appID, taskID)]
	s.mu.Unlock()
	if exec == nil {
		return false, nil
	}
	exec.cancelled.Store(true)
	// 先置取消标志再读取 cancel：若 cancel 尚未就绪，execute 的取消检查
	// 必然发生在 cancel 赋值之后，能观察到标志并持久化取消终态。
	exec.mu.Lock()
	cancel := exec.cancel
	exec.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true, nil
}

// CreateRequest 描述一个新任务。
type CreateRequest struct {
	AppID          string          // App 隔离边界
	Type           string          // 封闭任务类型
	Params         json.RawMessage // 参数（按注册 Schema 校验）
	IdempotencyKey string          // 幂等键
	MaxAttempts    uint32          // 执行/重试上限；0 使用调度器默认
	Deadline       time.Time       // 任务绝对截止时间
	AvailableAt    time.Time       // 可空：最早执行时间（定时/延迟）
	TaskID         string          // 可空：稳定标识，默认生成
}

func executionKey(appID, taskID string) string {
	return appID + "\x00" + taskID
}

func (s *Scheduler) detached(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), terminalWriteTimeout)
}

// retryDelay 计算指数退避延迟（带确定性抖动，有上限）。
func (s *Scheduler) retryDelay(value Task) time.Duration {
	delay := s.config.RetryBaseDelay
	for attempt := uint32(1); attempt < value.Attempt && delay < s.config.RetryMaxDelay; attempt++ {
		delay *= 2
		if delay > s.config.RetryMaxDelay {
			delay = s.config.RetryMaxDelay
		}
	}
	sum := sha256.Sum256([]byte(value.AppID + "/" + value.TaskID))
	jitterPermille := 800 + int(sum[0])*400/255
	return time.Duration(int64(delay) * int64(jitterPermille) / 1000)
}

// acquireSlots 非阻塞获取全局 worker 与 App 两级槽位。
func (s *Scheduler) acquireSlots(appID string) bool {
	select {
	case s.workers <- struct{}{}:
	default:
		return false
	}
	slots := s.appSlotsFor(appID)
	select {
	case slots.ch <- struct{}{}:
		return true
	default:
		<-s.workers
		return false
	}
}

func (s *Scheduler) releaseSlots(appID string) {
	<-s.appSlotsFor(appID).ch
	<-s.workers
}

func (s *Scheduler) appSlotsFor(appID string) *appSlots {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := s.appSlots[appID]
	if slots == nil {
		slots = &appSlots{ch: make(chan struct{}, s.config.AppCapacity)}
		s.appSlots[appID] = slots
	}
	return slots
}

func (s *Scheduler) transitionSucceeded(value Task) {
	now := s.now().UTC()
	ctx, cancel := s.detached(context.Background())
	defer cancel()
	if err := s.store.CompleteTask(ctx, value, now); err != nil {
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrLeaseLost) {
			observe.Error(ctx, "持久化后台任务成功终态失败", err,
				observe.StringAttr("app_id", value.AppID),
				observe.StringAttr("task_id", value.TaskID),
			)
		}
		return
	}
	observe.Info(ctx, "后台任务执行成功",
		observe.StringAttr("app_id", value.AppID),
		observe.StringAttr("task_id", value.TaskID),
		observe.StringAttr("task_type", value.Type),
	)
}

func (s *Scheduler) transitionFailure(value Task, errorClass ErrorClass) {
	now := s.now().UTC()
	ctx, cancel := s.detached(context.Background())
	defer cancel()
	if err := s.store.FailTask(ctx, value, errorClass, now); err != nil {
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrLeaseLost) {
			observe.Error(ctx, "持久化后台任务失败终态失败", err,
				observe.StringAttr("app_id", value.AppID),
				observe.StringAttr("task_id", value.TaskID),
			)
		}
		return
	}
	observe.Warn(ctx, "后台任务已失败",
		observe.StringAttr("app_id", value.AppID),
		observe.StringAttr("task_id", value.TaskID),
		observe.StringAttr("error_class", string(errorClass)),
	)
}

func (s *Scheduler) transitionRetry(value Task, errorClass ErrorClass) {
	now := s.now().UTC()
	nextAvailableAt := now.Add(s.retryDelay(value))
	if !nextAvailableAt.Before(value.Deadline) {
		// 退避后必超 deadline，重试没有意义，直接失败。
		s.transitionFailure(value, errorClass)
		return
	}
	ctx, cancel := s.detached(context.Background())
	defer cancel()
	if err := s.store.RetryTask(ctx, value, nextAvailableAt, now); err != nil {
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrLeaseLost) {
			observe.Error(ctx, "持久化后台任务重试安排失败", err,
				observe.StringAttr("app_id", value.AppID),
				observe.StringAttr("task_id", value.TaskID),
			)
		}
		return
	}
	observe.Warn(ctx, "后台任务已安排退避重试",
		observe.StringAttr("app_id", value.AppID),
		observe.StringAttr("task_id", value.TaskID),
		observe.IntAttr("next_attempt", int(value.Attempt+1)),
		observe.Int64Attr("available_unix_ms", nextAvailableAt.UnixMilli()),
	)
}

func (s *Scheduler) transitionCancelled(value Task) {
	now := s.now().UTC()
	ctx, cancel := s.detached(context.Background())
	defer cancel()
	if err := s.store.CancelRunningTask(ctx, value, now); err != nil {
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrLeaseLost) {
			observe.Error(ctx, "持久化后台任务取消终态失败", err,
				observe.StringAttr("app_id", value.AppID),
				observe.StringAttr("task_id", value.TaskID),
			)
		}
		return
	}
}

func (s *Scheduler) transitionLeaseLost(value Task) {
	if s.retryAllowed(value, ErrorClassLeaseLost) {
		s.transitionRetry(value, ErrorClassLeaseLost)
		return
	}
	s.transitionFailure(value, ErrorClassLeaseLost)
}
