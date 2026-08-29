package echo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Reader 是调度器读取 Echo 终态和访问层重放事件所需的最小端口。
type Reader interface {
	GetEcho(context.Context, string, string) (Record, []Event, error)
}

// Creator 是入口创建持久 Echo 所需的端口。
type Creator interface {
	CreateIdempotent(context.Context, RunRequest) (string, bool, error)
}

// Canceller 是入口取消持久 Echo 所需的端口。
type Canceller interface {
	Cancel(context.Context, string) (bool, error)
}

// SchedulerRunner 是持久 Run 调度器执行队列工作所需的编排端口。
type SchedulerRunner interface {
	RunQueued(context.Context, RunWork, EventEmitter) error
	Recoverable(context.Context) ([]RunWork, error)
	Runnable(context.Context, int) ([]RunWork, error)
	CancelQueuedRuns(context.Context) error
	Canceller
}

// EventSink 接收 Run 事件并在 Echo 结束时关闭实时订阅。
type EventSink interface {
	Publish(Event)
	Finish(string, string)
}

// Enqueuer 是入口通知持久调度器有新 Echo 的最小端口。
type Enqueuer interface {
	Enqueue(context.Context, string)
}

// SchedulerOption 配置持久 Run 调度器。
type SchedulerOption func(*Scheduler)

const (
	schedulerWorkers   = 4
	schedulerBatchSize = 32
	schedulerPoll      = 250 * time.Millisecond
)

// WithScheduler 配置持久 Run 调度器的 worker 数量、轮询周期与批大小。
func WithScheduler(workers int, poll time.Duration, batchSize int) SchedulerOption {
	return func(scheduler *Scheduler) {
		if workers > 0 {
			scheduler.workers = workers
		}
		if poll > 0 {
			scheduler.poll = poll
		}
		if batchSize > 0 {
			scheduler.batchSize = batchSize
		}
	}
}

type runKey struct {
	echoID string
	runID  string
}

// Scheduler 以持久化 queued Run 为事实源，负责固定并发、恢复、取消和事件发布。
// 入口只通过 Enqueue 提供低延迟提示；进程重启后仍由 Recover 从存储恢复。
type Scheduler struct {
	ctx        context.Context
	stop       context.CancelFunc
	runner     SchedulerRunner
	reader     Reader
	events     EventSink
	appID      string
	activeMu   sync.Mutex
	active     map[runKey]context.CancelFunc
	pending    map[string]context.Context
	activeWG   sync.WaitGroup
	workerWG   sync.WaitGroup
	work       chan struct{}
	startOnce  sync.Once
	shutdownMu sync.Mutex
	workers    int
	poll       time.Duration
	batchSize  int
}

// NewScheduler 构造 App 范围的持久 Run 调度器。
func NewScheduler(
	ctx context.Context,
	runner SchedulerRunner,
	reader Reader,
	events EventSink,
	appID string,
	options ...SchedulerOption,
) *Scheduler {
	if ctx == nil || runner == nil || reader == nil || events == nil || appID == "" {
		panic("scheduler dependencies are incomplete")
	}
	schedulerCtx, stop := context.WithCancel(ctx)
	scheduler := &Scheduler{
		ctx:       schedulerCtx,
		stop:      stop,
		runner:    runner,
		reader:    reader,
		events:    events,
		appID:     appID,
		active:    make(map[runKey]context.CancelFunc),
		pending:   make(map[string]context.Context),
		work:      make(chan struct{}, 1),
		workers:   schedulerWorkers,
		poll:      schedulerPoll,
		batchSize: schedulerBatchSize,
	}
	for _, option := range options {
		if option != nil {
			option(scheduler)
		}
	}
	return scheduler
}

// Recover 处理遗留 Run 并启动固定 worker。只有持久化 queued root 会重新进入队列。
func (s *Scheduler) Recover(ctx context.Context) (int, error) {
	work, err := s.runner.Recoverable(ctx)
	if err != nil {
		return 0, err
	}
	s.startOnce.Do(func() {
		for range s.workers {
			s.workerWG.Add(1)
			go s.worker()
		}
	})
	for _, item := range work {
		s.Enqueue(ctx, item.Run.EchoID)
	}
	if len(work) > 0 {
		observe.Info(ctx, "已重新调度持久化的排队 Run",
			observe.StringAttr("app_id", s.appID),
			observe.IntAttr("run_count", len(work)),
		)
	}
	return len(work), nil
}

// Enqueue 记录入口上下文并唤醒一个 worker；实际工作仍以数据库队列为准。
func (s *Scheduler) Enqueue(parent context.Context, echoID string) {
	if echoID == "" || s.ctx.Err() != nil {
		return
	}
	runContext := observe.Copy(parent, s.ctx)
	s.activeMu.Lock()
	active := false
	for run := range s.active {
		if run.echoID == echoID {
			active = true
			break
		}
	}
	if !active {
		s.pending[echoID] = runContext
	}
	s.activeMu.Unlock()
	s.signal()
}

// Cancel 持久化取消排队 Run，并中止调度器中已经领取的 Run。
func (s *Scheduler) Cancel(ctx context.Context, echoID string) (bool, error) {
	cancelled, err := s.runner.Cancel(ctx, echoID)
	if err != nil {
		return false, err
	}
	s.activeMu.Lock()
	delete(s.pending, echoID)
	cancellations := make([]context.CancelFunc, 0, 2)
	for key, cancel := range s.active {
		if key.echoID == echoID {
			cancellations = append(cancellations, cancel)
		}
	}
	s.activeMu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
	if cancelled && len(cancellations) == 0 {
		s.finishEchoIfTerminal(ctx, echoID)
	}
	return cancelled || len(cancellations) > 0, nil
}

func (s *Scheduler) worker() {
	defer s.workerWG.Done()
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.work:
		case <-ticker.C:
		}
		for s.runNext() {
		}
	}
}

func (s *Scheduler) runNext() bool {
	if s.ctx.Err() != nil {
		return false
	}
	work, err := s.runner.Runnable(s.ctx, s.batchSize)
	if err != nil {
		if s.ctx.Err() == nil {
			observe.Error(s.ctx, "读取持久 Run 队列失败", err,
				observe.StringAttr("app_id", s.appID),
			)
		}
		return false
	}
	var selected *RunWork
	var runContext context.Context
	var cancel context.CancelFunc
	s.activeMu.Lock()
	if s.ctx.Err() == nil {
		for index := range work {
			key := runKey{echoID: work[index].Run.EchoID, runID: work[index].Run.ID}
			if _, running := s.active[key]; running {
				continue
			}
			selected = &work[index]
			base := s.ctx
			if pendingContext, exists := s.pending[key.echoID]; exists && work[index].Run.ParentRunID == "" {
				base = observe.Copy(pendingContext, s.ctx)
				delete(s.pending, key.echoID)
			}
			runContext = observe.With(base,
				observe.StringAttr("app_id", s.appID),
				observe.StringAttr("echo_id", key.echoID),
				observe.StringAttr("run_id", key.runID),
			)
			runContext, cancel = context.WithCancel(runContext)
			s.active[key] = cancel
			s.activeWG.Add(1)
			break
		}
	}
	s.activeMu.Unlock()
	if selected == nil {
		return false
	}
	key := runKey{echoID: selected.Run.EchoID, runID: selected.Run.ID}
	emit := func(event Event) error {
		s.events.Publish(event)
		return nil
	}
	runErr := s.runner.RunQueued(runContext, *selected, emit)
	s.activeMu.Lock()
	delete(s.active, key)
	s.activeMu.Unlock()
	s.finishEchoIfTerminal(runContext, key.echoID)
	cancel()
	s.activeWG.Done()
	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, ErrInvalidTransition) &&
		!errors.Is(runErr, ErrRunRetryScheduled) {
		observe.Error(runContext, "持久调度 Run 执行失败", runErr)
	}
	s.signal()
	return true
}

func (s *Scheduler) finishEchoIfTerminal(ctx context.Context, echoID string) {
	readContext, cancel := detachedContext(ctx)
	defer cancel()
	record, _, err := s.reader.GetEcho(readContext, s.appID, echoID)
	if err != nil {
		observe.Error(ctx, "Run 结束后读取 Echo 状态失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		return
	}
	if record.Status != StatusRunning {
		s.events.Finish(s.appID, echoID)
	}
}

func (s *Scheduler) signal() {
	select {
	case s.work <- struct{}{}:
	default:
	}
}

// Shutdown 停止 worker，取消持久队列中的 Echo，并等待活动 Run 结束。
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	return s.shutdown(ctx)
}

func (s *Scheduler) shutdown(ctx context.Context) error {
	s.stop()
	s.activeMu.Lock()
	activeEchoIDs := make(map[string]struct{}, len(s.active))
	activeCancellations := make([]context.CancelFunc, 0, len(s.active))
	for key := range s.active {
		activeEchoIDs[key.echoID] = struct{}{}
	}
	for _, cancel := range s.active {
		activeCancellations = append(activeCancellations, cancel)
	}
	clear(s.pending)
	s.activeMu.Unlock()
	for _, cancel := range activeCancellations {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		s.workerWG.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("等待活动 Run 停止：%w", ctx.Err())
	case <-done:
	}
	var cancellationErrors []error
	if err := s.runner.CancelQueuedRuns(ctx); err != nil {
		cancellationErrors = append(cancellationErrors, fmt.Errorf("持久化取消排队 Echo：%w", err))
	}
	for echoID := range activeEchoIDs {
		if _, err := s.Cancel(ctx, echoID); err != nil && !errors.Is(err, ErrInvalidTransition) {
			cancellationErrors = append(cancellationErrors, fmt.Errorf("持久化取消 Echo %s：%w", echoID, err))
		}
	}
	return errors.Join(cancellationErrors...)
}
