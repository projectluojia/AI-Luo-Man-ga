package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

//go:embed static/*
var staticFiles embed.FS

type EchoReader interface {
	GetEcho(context.Context, string, string) (kernelecho.Record, []kernelecho.Event, error)
	ListRuns(context.Context, string, string) ([]kernelecho.RunRecord, error)
}

type runReader interface {
	GetRun(context.Context, string, string) (kernelecho.RunRecord, error)
}

type HealthChecker interface {
	Ping(context.Context) error
}

type EchoOrchestrator interface {
	CreateIdempotent(context.Context, kernelecho.RunRequest) (string, bool, error)
	RunExisting(context.Context, string, kernelecho.RunRequest, kernelecho.EventEmitter) error
	Recoverable(context.Context) ([]kernelecho.RunWork, error)
	Runnable(context.Context, int) ([]kernelecho.RunWork, error)
	Cancel(context.Context, string) (bool, error)
}

type queuedRunOrchestrator interface {
	RunQueued(context.Context, kernelecho.RunWork, kernelecho.EventEmitter) error
}

// ConfirmationGateway 是确认决策公共 HTTP 端口的依赖视图：状态查询、原子决策
// 与 Echo 内活跃确认列表。由 confirmation.Service 实现。
type ConfirmationGateway interface {
	Resolve(ctx context.Context, appID, confirmationID string) (confirmation.Confirmation, error)
	Decide(ctx context.Context, appID, confirmationID, decision, confirmedBy string, decidedAt time.Time) (confirmation.Confirmation, error)
	ActiveByEcho(ctx context.Context, appID, echoID string) ([]confirmation.Confirmation, error)
}

type Server struct {
	schedulerCtx       context.Context
	stopSchedule       context.CancelFunc
	orchestrator       EchoOrchestrator
	reader             EchoReader
	health             HealthChecker
	registry           *registry.Registry
	policy             runtime.AppPolicy
	appID              string
	platformHub        *access.Hub
	webAuthenticator   WebAuthenticator
	qqAccessAdmin      QQAccessAdmin
	confirmations      ConfirmationGateway
	hub                *access.EventHub
	activeMu           sync.Mutex
	active             map[runKey]context.CancelFunc
	pending            map[echoKey]context.Context
	activeWG           sync.WaitGroup
	workerWG           sync.WaitGroup
	admissionWG        sync.WaitGroup
	workSignal         chan struct{}
	scheduleOnce       sync.Once
	accepting          bool
	schedulerWorkers   int
	schedulerPoll      time.Duration
	schedulerBatchSize int
}

const (
	schedulerWorkers   = 4
	schedulerBatchSize = 32
	schedulerPoll      = 250 * time.Millisecond
)

// WithScheduler 配置持久 Run 调度器的 worker 数量、轮询周期与批大小。
func WithScheduler(workers int, poll time.Duration, batchSize int) ServerOption {
	return func(server *Server) {
		if workers > 0 {
			server.schedulerWorkers = workers
		}
		if poll > 0 {
			server.schedulerPoll = poll
		}
		if batchSize > 0 {
			server.schedulerBatchSize = batchSize
		}
	}
}

// echoKey 是活动/待处理 Echo 的进程内键。
type echoKey struct {
	appID  string
	echoID string
}

// runKey 允许同一 Echo 的 root 与 child Run 同时处于活动状态。
type runKey struct {
	appID  string
	echoID string
	runID  string
}

func NewServer(
	ctx context.Context,
	orchestrator EchoOrchestrator,
	reader EchoReader,
	health HealthChecker,
	reg *registry.Registry,
	policy runtime.AppPolicy,
	appID string,
	platformHub *access.Hub,
	options ...ServerOption,
) *Server {
	schedulerCtx, stopSchedule := context.WithCancel(ctx)
	server := &Server{
		schedulerCtx:       schedulerCtx,
		stopSchedule:       stopSchedule,
		orchestrator:       orchestrator,
		reader:             reader,
		health:             health,
		registry:           reg,
		policy:             policy,
		appID:              appID,
		platformHub:        platformHub,
		hub:                access.NewEventHub(),
		active:             make(map[runKey]context.CancelFunc),
		pending:            make(map[echoKey]context.Context),
		workSignal:         make(chan struct{}, 1),
		accepting:          true,
		schedulerWorkers:   schedulerWorkers,
		schedulerPoll:      schedulerPoll,
		schedulerBatchSize: schedulerBatchSize,
	}
	for _, option := range options {
		if option != nil {
			option(server)
		}
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", observe.DefaultMetrics())
	mux.HandleFunc("GET /api/v1/capabilities", s.capabilities)
	mux.HandleFunc("GET /api/v1/admin/qq-access", s.getQQAccess)
	mux.HandleFunc("PUT /api/v1/admin/qq-access", s.updateQQAccess)
	mux.HandleFunc("POST /api/v2/echoes", s.createEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}", s.getEcho)
	mux.HandleFunc("GET /api/v1/runs/{run_id}", s.getRun)
	mux.HandleFunc("DELETE /api/v1/echoes/{echo_id}", s.cancelEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}/events", s.echoEvents)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}/confirmations", s.listConfirmations)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}/confirmations/{confirmation_id}", s.getConfirmation)
	mux.HandleFunc("POST /api/v1/echoes/{echo_id}/confirmations/{confirmation_id}/decision", s.decideConfirmation)
	// 产品前端聊天契约（LuoYing-Frontend）：流式接口，经标准 Intake → Echo →
	// 事件翻译链路，不直接暴露内核事件类型。
	mux.HandleFunc("POST /chat/stream", s.chatStream)
	static, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return observe.HTTPMiddleware("web_access", access.SecurityHeaders(mux))
}

func (s *Server) Recover(ctx context.Context) (int, error) {
	work, err := s.orchestrator.Recoverable(ctx)
	if err != nil {
		return 0, err
	}
	s.activateScheduler()
	for _, item := range work {
		s.queueEcho(ctx, item.Run.EchoID)
	}
	if len(work) > 0 {
		observe.Info(ctx, "已重新调度持久化的排队 Run",
			observe.StringAttr("app_id", s.appID),
			observe.IntAttr("run_count", len(work)),
		)
	}
	return len(work), nil
}

func (s *Server) healthz(writer http.ResponseWriter, request *http.Request) {
	access.WriteJSON(writer, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Server) readyz(writer http.ResponseWriter, request *http.Request) {
	s.activeMu.Lock()
	accepting := s.accepting
	s.activeMu.Unlock()
	if !accepting {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "shutting_down", "message": "服务正在关闭"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.health.Ping(ctx); err != nil {
		observe.Error(request.Context(), "服务健康检查失败", err)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "dependency_unavailable", "message": "服务依赖暂时不可用"})
		return
	}
	access.WriteJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) capabilities(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := s.policy.Snapshot(request.Context(), s.appID)
	if err != nil {
		observe.Error(request.Context(), "读取 App Capability 策略失败", err,
			observe.StringAttr("app_id", s.appID),
		)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"code": "app_policy_unavailable", "message": "当前 App 策略暂时不可用",
		})
		return
	}
	if err := snapshot.Verify(s.appID); err != nil {
		observe.Error(request.Context(), "App Capability 策略身份不匹配", runtime.ErrAppPolicyUnavailable,
			observe.StringAttr("app_id", s.appID),
		)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"code": "app_policy_unavailable", "message": "当前 App 策略暂时不可用",
		})
		return
	}
	if !snapshot.Enabled {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"code": "app_disabled", "message": "当前 App 已停用",
		})
		return
	}
	items := make([]registry.CapabilitySpec, 0)
	for _, capability := range s.registry.Capabilities() {
		if snapshot.CapabilityEnabled(capability.ID) {
			items = append(items, capability)
		}
	}
	observe.Debug(request.Context(), "已返回当前 App 的能力清单",
		observe.StringAttr("app_id", s.appID),
		observe.IntAttr("capability_count", len(items)),
	)
	access.WriteJSON(writer, http.StatusOK, map[string]any{"capabilities": items})
}

func (s *Server) createEcho(writer http.ResponseWriter, request *http.Request) {
	if !s.beginAdmission() {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "shutting_down", "message": "服务正在关闭"})
		return
	}
	defer s.admissionWG.Done()
	webIdentity, authenticated := s.authenticateWeb(writer, request)
	if !authenticated {
		return
	}
	idempotencyValues := request.Header.Values("Idempotency-Key")
	if len(idempotencyValues) != 1 || idempotencyValues[0] != strings.TrimSpace(idempotencyValues[0]) || idempotency.ValidateKey(idempotencyValues[0]) != nil {
		observe.Warn(request.Context(), "创建 Echo 的幂等键无效")
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_idempotency_key", "message": "Idempotency-Key 必须是 1 至 128 位安全字符"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input kernelecho.RunRequest
	if err := decoder.Decode(&input); err != nil {
		observe.Warn(request.Context(), "创建 Echo 的请求体解析失败",
			observe.StringAttr("reason", err.Error()),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体不是有效的 JSON 对象"})
		return
	}
	if err := jsonutil.EnsureEOF(decoder); err != nil {
		observe.Warn(request.Context(), "创建 Echo 的请求体包含多余内容",
			observe.StringAttr("reason", err.Error()),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体只能包含一个 JSON 对象"})
		return
	}
	input.Message = strings.TrimSpace(input.Message)
	if input.Message == "" || utf8.RuneCountInString(input.Message) > 4000 {
		observe.Warn(request.Context(), "创建 Echo 的消息长度不合法",
			observe.IntAttr("message_length", utf8.RuneCountInString(input.Message)),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_message", "message": "消息长度必须为 1 至 4000 个字符"})
		return
	}
	input.IdempotencyKey = idempotencyValues[0]
	// 平台接入统一入口：标准消息校验 → 身份解析 → 会话找到或创建 → 消息入库。
	// 平台与 Agent 历史在此解耦：消息进会话台账（SQLite），Echo 在入库成功后创建。
	intake, err := s.platformHub.Intake(request.Context(), access.InboundMessage{
		AppID:             s.appID,
		Platform:          "web",
		PlatformChannel:   "private",
		PlatformSpaceID:   webIdentity.PlatformSpaceID,
		PlatformUserID:    webIdentity.PlatformUserID,
		PlatformMessageID: input.IdempotencyKey,
		PlatformSessionID: webIdentity.PlatformSessionID,
		MessageType:       "text",
		Text:              input.Message,
		OccurredAt:        time.Now().UTC(),
		IdempotencyKey:    input.IdempotencyKey,
	})
	if err != nil {
		access.WriteIntakeError(writer, request, err)
		return
	}
	// 会话上下文只来自受治理的 Intake 结果，覆盖客户端请求体中的任何同名字段
	// （RunRequest 的会话字段不进入 HTTP 契约，客户端无法伪造会话归属）。
	input.Message = intake.Text
	input.SessionID = intake.SessionID
	input.UserID = intake.UserID
	input.MessageID = intake.MessageID
	echoID, created, err := s.orchestrator.CreateIdempotent(request.Context(), input)
	if err != nil {
		access.WriteEchoError(writer, request, err)
		return
	}
	if created {
		s.queueEcho(request.Context(), echoID)
	} else {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	runContext := observe.With(request.Context(),
		observe.StringAttr("app_id", s.appID),
		observe.StringAttr("echo_id", echoID),
	)
	observe.Info(runContext, "Echo 创建请求已处理",
		observe.IntAttr("message_length", utf8.RuneCountInString(input.Message)),
		observe.BoolAttr("idempotency_replayed", !created),
	)
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	access.WriteJSON(writer, status, map[string]string{
		"echo_id":    echoID,
		"status_url": "/api/v1/echoes/" + echoID,
		"events_url": "/api/v1/echoes/" + echoID + "/events",
	})
}

func (s *Server) queueEcho(parent context.Context, echoID string) {
	s.activateScheduler()
	key := echoKey{appID: s.appID, echoID: echoID}
	runContext := observe.Copy(parent, s.schedulerCtx)
	s.activeMu.Lock()
	active := false
	for run := range s.active {
		if run.appID == key.appID && run.echoID == key.echoID {
			active = true
			break
		}
	}
	if !active {
		s.pending[key] = runContext
	}
	s.activeMu.Unlock()
	s.signalWork()
}

func (s *Server) startWorkers() {
	for worker := 0; worker < s.schedulerWorkers; worker++ {
		s.workerWG.Add(1)
		go s.worker()
	}
}

func (s *Server) worker() {
	defer s.workerWG.Done()
	ticker := time.NewTicker(s.schedulerPoll)
	defer ticker.Stop()
	for {
		select {
		case <-s.schedulerCtx.Done():
			return
		case <-s.workSignal:
		case <-ticker.C:
		}
		for s.runNext() {
		}
	}
}

func (s *Server) activateScheduler() {
	s.scheduleOnce.Do(func() {
		s.startWorkers()
	})
}

func (s *Server) runNext() bool {
	work, err := s.orchestrator.Runnable(s.schedulerCtx, s.schedulerBatchSize)
	if err != nil {
		if s.schedulerCtx.Err() == nil {
			observe.Error(s.schedulerCtx, "读取持久 Run 队列失败", err,
				observe.StringAttr("app_id", s.appID),
			)
		}
		return false
	}
	var selected *kernelecho.RunWork
	var runContext context.Context
	var cancel context.CancelFunc
	s.activeMu.Lock()
	if s.schedulerCtx.Err() == nil {
		for index := range work {
			key := runKey{appID: s.appID, echoID: work[index].Run.EchoID, runID: work[index].Run.ID}
			if _, running := s.active[key]; running {
				continue
			}
			selected = &work[index]
			base := s.schedulerCtx
			echo := echoKey{appID: key.appID, echoID: key.echoID}
			if pendingContext, exists := s.pending[echo]; exists && work[index].Run.ParentRunID == "" {
				base = observe.Copy(pendingContext, s.schedulerCtx)
				delete(s.pending, echo)
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
	key := runKey{appID: s.appID, echoID: selected.Run.EchoID, runID: selected.Run.ID}
	emit := func(event kernelecho.Event) error {
		s.hub.Publish(event)
		return nil
	}
	var runErr error
	if queued, ok := s.orchestrator.(queuedRunOrchestrator); ok {
		runErr = queued.RunQueued(runContext, *selected, emit)
	} else {
		runErr = s.orchestrator.RunExisting(runContext, key.echoID, kernelecho.RunRequest{Message: selected.InputMessage}, emit)
	}
	s.activeMu.Lock()
	delete(s.active, key)
	s.activeMu.Unlock()
	s.activeWG.Done()
	s.finishEchoIfTerminal(runContext, key.echoID)
	cancel()
	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, kernelecho.ErrInvalidTransition) &&
		!errors.Is(runErr, kernelecho.ErrRunRetryScheduled) {
		observe.Error(runContext, "持久调度 Run 执行失败", runErr)
	}
	s.signalWork()
	return true
}

func (s *Server) finishEchoIfTerminal(ctx context.Context, echoID string) {
	record, _, err := s.reader.GetEcho(ctx, s.appID, echoID)
	if err != nil {
		observe.Error(ctx, "Run 结束后读取 Echo 状态失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		return
	}
	if record.Status != kernelecho.StatusRunning {
		s.hub.Finish(s.appID, echoID)
	}
}

func (s *Server) signalWork() {
	select {
	case s.workSignal <- struct{}{}:
	default:
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.StopAccepting()
	admissionDone := make(chan struct{})
	go func() {
		s.admissionWG.Wait()
		close(admissionDone)
	}()
	select {
	case <-admissionDone:
	case <-ctx.Done():
		return fmt.Errorf("等待 Echo 接入事务完成：%w", ctx.Err())
	}
	s.stopSchedule()
	s.activeMu.Lock()
	cancellations := make([]context.CancelFunc, 0, len(s.active))
	echoIDSet := make(map[string]struct{}, len(s.active)+len(s.pending))
	for key, cancel := range s.active {
		cancellations = append(cancellations, cancel)
		echoIDSet[key.echoID] = struct{}{}
	}
	for key := range s.pending {
		echoIDSet[key.echoID] = struct{}{}
	}
	s.activeMu.Unlock()
	if queued, err := s.orchestrator.Runnable(ctx, 1000); err != nil {
		return fmt.Errorf("读取关闭时排队 Run：%w", err)
	} else {
		for _, item := range queued {
			echoIDSet[item.Run.EchoID] = struct{}{}
		}
	}
	var cancellationErrors []error
	for echoID := range echoIDSet {
		if _, err := s.orchestrator.Cancel(ctx, echoID); err != nil {
			if !errors.Is(err, kernelecho.ErrInvalidTransition) {
				cancellationErrors = append(cancellationErrors, fmt.Errorf("持久化取消 Echo %s：%w", echoID, err))
			}
		}
	}
	for _, cancel := range cancellations {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		s.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return errors.Join(cancellationErrors...)
	case <-ctx.Done():
		return errors.Join(append(cancellationErrors, fmt.Errorf("等待活动 Run 停止：%w", ctx.Err()))...)
	}
}

func (s *Server) StopAccepting() {
	s.activeMu.Lock()
	s.accepting = false
	s.activeMu.Unlock()
}

// Hub 返回共享的 Echo 事件订阅中心，供平台适配器（QQ 等）订阅 Run 事件回发。
func (s *Server) Hub() *access.EventHub {
	return s.hub
}

func (s *Server) beginAdmission() bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if !s.accepting {
		return false
	}
	s.admissionWG.Add(1)
	return true
}

func (s *Server) getEcho(writer http.ResponseWriter, request *http.Request) {
	echoID := request.PathValue("echo_id")
	record, events, err := s.reader.GetEcho(request.Context(), s.appID, echoID)
	if err != nil {
		if !errors.Is(err, kernelecho.ErrEchoNotFound) {
			observe.Error(request.Context(), "读取 Echo 状态失败", err,
				observe.StringAttr("app_id", s.appID),
				observe.StringAttr("echo_id", echoID),
			)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 状态读取失败"})
			return
		}
		observe.Warn(request.Context(), "查询的 Echo 不存在",
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "echo_not_found", "message": "Echo 不存在"})
		return
	}
	runs, err := s.reader.ListRuns(request.Context(), s.appID, echoID)
	if err != nil {
		observe.Error(request.Context(), "读取 Echo 的 Run 状态失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 状态读取失败"})
		return
	}
	publicRuns := make([]kernelecho.PublicRun, 0, len(runs))
	for _, run := range runs {
		publicRuns = append(publicRuns, kernelecho.PublicRunRecord(run))
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"echo": record, "runs": publicRuns, "events": events})
}

func (s *Server) getRun(writer http.ResponseWriter, request *http.Request) {
	reader, ok := s.reader.(runReader)
	if !ok {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "run_status_unavailable", "message": "Run 状态查询暂不可用"})
		return
	}
	runID := request.PathValue("run_id")
	run, err := reader.GetRun(request.Context(), s.appID, runID)
	if err != nil {
		if errors.Is(err, kernelecho.ErrRunNotFound) {
			access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "run_not_found", "message": "Run 不存在"})
			return
		}
		observe.Error(request.Context(), "读取 Run 状态失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("run_id", runID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Run 状态读取失败"})
		return
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"run": kernelecho.PublicRunRecord(run)})
}

func (s *Server) cancelEcho(writer http.ResponseWriter, request *http.Request) {
	echoID := request.PathValue("echo_id")
	if _, _, err := s.reader.GetEcho(request.Context(), s.appID, echoID); err != nil {
		if !errors.Is(err, kernelecho.ErrEchoNotFound) {
			observe.Error(request.Context(), "取消前读取 Echo 状态失败", err,
				observe.StringAttr("app_id", s.appID),
				observe.StringAttr("echo_id", echoID),
			)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 状态读取失败"})
			return
		}
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "echo_not_found", "message": "Echo 不存在"})
		return
	}
	cancelledQueued, err := s.orchestrator.Cancel(request.Context(), echoID)
	if err != nil {
		if errors.Is(err, kernelecho.ErrInvalidTransition) {
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "echo_not_running", "message": "Echo 当前不在运行"})
			return
		}
		observe.Error(request.Context(), "持久化 Echo 取消状态失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 取消失败"})
		return
	}
	s.activeMu.Lock()
	cancellations := make([]context.CancelFunc, 0, 2)
	for key, cancel := range s.active {
		if key.appID == s.appID && key.echoID == echoID {
			cancellations = append(cancellations, cancel)
		}
	}
	s.activeMu.Unlock()
	if len(cancellations) == 0 && !cancelledQueued {
		observe.Warn(request.Context(), "无法取消未运行的 Echo",
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "echo_not_running", "message": "Echo 当前不在运行"})
		return
	}
	for _, cancel := range cancellations {
		cancel()
	}
	observe.Info(request.Context(), "已请求取消 Echo",
		observe.StringAttr("echo_id", echoID),
	)
	writer.WriteHeader(http.StatusNoContent)
}

// publicConfirmation 是确认记录的公共响应视图：只暴露呈现与决策所需字段，
// publicConfirmation converts a confirmation record into its public response fields,
// publicConfirmation converts a confirmation record to its public response representation,
// including decision metadata when available.
func publicConfirmation(record confirmation.Confirmation, effectiveStatus string) map[string]any {
	view := map[string]any{
		"confirmation_id": record.ConfirmationID,
		"echo_id":         record.EchoID,
		"capability_id":   record.CapabilityID,
		"target_type":     record.TargetType,
		"target_id":       record.TargetID,
		"side_effect":     record.SideEffect,
		"status":          effectiveStatus,
		"expires_at":      record.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at":      record.CreatedAt.UTC().Format(time.RFC3339),
	}
	if record.ConfirmedBy != "" {
		view["confirmed_by"] = record.ConfirmedBy
	}
	if record.DecidedAt != nil {
		view["decided_at"] = record.DecidedAt.UTC().Format(time.RFC3339)
	}
	return view
}

// loadConfirmation 读取确认记录并强制 Echo 归属匹配：跨 Echo 的确认标识按
// 不存在处理（fail-closed，不泄露记录是否存在）——Echo 归属校验先于过期判定，
// 过期记录的元数据同样不得泄露给其他 Echo 的调用方。已过有效期（含未显式过期）
// 的记录按 expired 状态返回，便于界面呈现"已失效"。
func (s *Server) loadConfirmation(writer http.ResponseWriter, request *http.Request, echoID, confirmationID string) (confirmation.Confirmation, string, bool) {
	record, err := s.confirmations.Resolve(request.Context(), s.appID, confirmationID)
	// Resolve 对已过期记录仍返回记录本体（err=ErrExpired）；无论是否过期，
	// 都先做 Echo 归属校验再决定可见性。
	if err != nil && !errors.Is(err, confirmation.ErrExpired) {
		observe.Warn(request.Context(), "查询的确认记录不存在",
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "confirmation_not_found", "message": "确认记录不存在"})
		return confirmation.Confirmation{}, "", false
	}
	if record.EchoID != echoID {
		observe.Warn(request.Context(), "确认记录与 Echo 不匹配",
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "confirmation_not_found", "message": "确认记录不存在"})
		return confirmation.Confirmation{}, "", false
	}
	if err != nil {
		return record, confirmation.StatusExpired, true
	}
	return record, record.Status, true
}

func (s *Server) listConfirmations(writer http.ResponseWriter, request *http.Request) {
	if s.confirmations == nil {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "confirmation_unavailable", "message": "确认治理暂不可用"})
		return
	}
	echoID := request.PathValue("echo_id")
	if _, _, err := s.reader.GetEcho(request.Context(), s.appID, echoID); err != nil {
		if !errors.Is(err, kernelecho.ErrEchoNotFound) {
			observe.Error(request.Context(), "读取确认列表前读取 Echo 状态失败", err,
				observe.StringAttr("echo_id", echoID),
			)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "确认记录读取失败"})
			return
		}
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "echo_not_found", "message": "Echo 不存在"})
		return
	}
	records, err := s.confirmations.ActiveByEcho(request.Context(), s.appID, echoID)
	if err != nil {
		observe.Error(request.Context(), "读取 Echo 活跃确认失败", err,
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "确认记录读取失败"})
		return
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		items = append(items, publicConfirmation(record, record.Status))
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"confirmations": items})
}

func (s *Server) getConfirmation(writer http.ResponseWriter, request *http.Request) {
	if s.confirmations == nil {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "confirmation_unavailable", "message": "确认治理暂不可用"})
		return
	}
	echoID := request.PathValue("echo_id")
	record, status, ok := s.loadConfirmation(writer, request, echoID, request.PathValue("confirmation_id"))
	if !ok {
		return
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"confirmation": publicConfirmation(record, status)})
}

func (s *Server) decideConfirmation(writer http.ResponseWriter, request *http.Request) {
	if s.confirmations == nil {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "confirmation_unavailable", "message": "确认治理暂不可用"})
		return
	}
	echoID := request.PathValue("echo_id")
	// 决策归因来自服务端可信登录态，不接受客户端自报：决策人字段不可伪造。
	webIdentity, authenticated := s.authenticateWeb(writer, request)
	if !authenticated {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Decision string `json:"decision"`
	}
	if err := decoder.Decode(&input); err != nil || jsonutil.EnsureEOF(decoder) != nil {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体不是有效的 JSON 对象"})
		return
	}
	if input.Decision != confirmation.StatusApproved && input.Decision != confirmation.StatusRejected {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{
			"code":    "invalid_request",
			"message": "decision 必须是 approved 或 rejected",
		})
		return
	}
	// Echo 归属校验：确认必须属于该 Echo（过期记录不可决策，Resolve 已拦）。
	if _, _, ok := s.loadConfirmation(writer, request, echoID, request.PathValue("confirmation_id")); !ok {
		return
	}
	confirmationID := request.PathValue("confirmation_id")
	record, err := s.confirmations.Decide(request.Context(), s.appID, confirmationID,
		input.Decision, webIdentity.PlatformUserID, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, confirmation.ErrNotFound):
			access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "confirmation_not_found", "message": "确认记录不存在"})
		case errors.Is(err, confirmation.ErrExpired):
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "confirmation_expired", "message": "确认记录已过期"})
		case errors.Is(err, confirmation.ErrRevoked):
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "confirmation_revoked", "message": "确认记录已撤销"})
		case errors.Is(err, confirmation.ErrAlreadyDecided):
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "confirmation_already_decided", "message": "确认记录已决策"})
		case errors.Is(err, confirmation.ErrInvalidRequest):
			access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "决策请求字段不合法"})
		default:
			observe.Error(request.Context(), "持久化确认决策失败", err,
				observe.StringAttr("echo_id", echoID),
			)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "确认决策失败"})
		}
		return
	}
	observe.Info(request.Context(), "确认决策已提交",
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("confirmation_id", confirmationID),
		observe.StringAttr("decision", record.Status),
	)
	access.WriteJSON(writer, http.StatusOK, map[string]any{"confirmation": publicConfirmation(record, record.Status)})
}

func (s *Server) echoEvents(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		observe.Error(request.Context(), "当前响应实现不支持 SSE", nil)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "streaming_unavailable"})
		return
	}
	echoID := request.PathValue("echo_id")
	_, _, err := s.reader.GetEcho(request.Context(), s.appID, echoID)
	if err != nil {
		if !errors.Is(err, kernelecho.ErrEchoNotFound) {
			observe.Error(request.Context(), "订阅前读取 Echo 事件失败", err,
				observe.StringAttr("app_id", s.appID),
				observe.StringAttr("echo_id", echoID),
			)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 事件读取失败"})
			return
		}
		observe.Warn(request.Context(), "订阅事件时 Echo 不存在",
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "echo_not_found", "message": "Echo 不存在"})
		return
	}
	live, unsubscribe := s.hub.Subscribe(s.appID, echoID)
	defer unsubscribe()
	record, events, err := s.reader.GetEcho(request.Context(), s.appID, echoID)
	if err != nil {
		observe.Error(request.Context(), "订阅建立后重放 Echo 事件失败", err,
			observe.StringAttr("app_id", s.appID),
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Echo 事件读取失败"})
		return
	}
	lastSequence, _ := strconv.ParseUint(request.Header.Get("Last-Event-ID"), 10, 64)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	observe.Debug(request.Context(), "开始推送 Echo 事件",
		observe.StringAttr("echo_id", echoID),
		observe.Int64Attr("last_sequence", int64(lastSequence)),
		observe.IntAttr("replay_event_count", len(events)),
	)
	for _, event := range events {
		if event.Sequence > lastSequence {
			if err := writeSSE(writer, event); err != nil {
				observe.Warn(request.Context(), "SSE 客户端连接已中断",
					observe.StringAttr("app_id", s.appID),
					observe.StringAttr("echo_id", echoID),
					observe.Int64Attr("event_sequence", int64(event.Sequence)),
				)
				return
			}
			lastSequence = event.Sequence
		}
	}
	flusher.Flush()
	if record.Status != kernelecho.StatusRunning {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			observe.Debug(request.Context(), "Echo 事件订阅连接已经断开",
				observe.StringAttr("echo_id", echoID),
			)
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-live:
			if !open {
				return
			}
			if event.Sequence <= lastSequence {
				continue
			}
			if err := writeSSE(writer, event); err != nil {
				return
			}
			lastSequence = event.Sequence
			flusher.Flush()
		}
	}
}

func writeSSE(writer io.Writer, event kernelecho.Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, encoded)
	return err
}
