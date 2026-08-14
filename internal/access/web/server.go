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

type Server struct {
	schedulerCtx     context.Context
	stopSchedule     context.CancelFunc
	orchestrator     EchoOrchestrator
	reader           EchoReader
	health           HealthChecker
	registry         *registry.Registry
	policy           runtime.AppPolicy
	appID            string
	platformHub      *access.Hub
	webAuthenticator WebAuthenticator
	hub              *access.EventHub
	activeMu         sync.Mutex
	active           map[echoKey]context.CancelFunc
	pending          map[echoKey]context.Context
	activeWG         sync.WaitGroup
	workerWG         sync.WaitGroup
	admissionWG      sync.WaitGroup
	workSignal       chan struct{}
	scheduleOnce     sync.Once
	accepting        bool
}

const (
	schedulerWorkers   = 4
	schedulerBatchSize = 32
	schedulerPoll      = 250 * time.Millisecond
)

// echoKey 是活动/待处理 Echo 的进程内键。
type echoKey struct {
	appID  string
	echoID string
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
		schedulerCtx: schedulerCtx,
		stopSchedule: stopSchedule,
		orchestrator: orchestrator,
		reader:       reader,
		health:       health,
		registry:     reg,
		policy:       policy,
		appID:        appID,
		platformHub:  platformHub,
		hub:          access.NewEventHub(),
		active:       make(map[echoKey]context.CancelFunc),
		pending:      make(map[echoKey]context.Context),
		workSignal:   make(chan struct{}, 1),
		accepting:    true,
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
	mux.HandleFunc("POST /api/v2/echoes", s.createEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}", s.getEcho)
	mux.HandleFunc("DELETE /api/v1/echoes/{echo_id}", s.cancelEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}/events", s.echoEvents)
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
	if _, active := s.active[key]; !active {
		s.pending[key] = runContext
	}
	s.activeMu.Unlock()
	s.signalWork()
}

func (s *Server) startWorkers() {
	for worker := 0; worker < schedulerWorkers; worker++ {
		s.workerWG.Add(1)
		go s.worker()
	}
}

func (s *Server) worker() {
	defer s.workerWG.Done()
	ticker := time.NewTicker(schedulerPoll)
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
	work, err := s.orchestrator.Runnable(s.schedulerCtx, schedulerBatchSize)
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
			key := echoKey{appID: s.appID, echoID: work[index].Run.EchoID}
			if _, running := s.active[key]; running {
				continue
			}
			selected = &work[index]
			base := s.schedulerCtx
			if pendingContext, exists := s.pending[key]; exists {
				base = observe.Copy(pendingContext, s.schedulerCtx)
				delete(s.pending, key)
			}
			runContext = observe.With(base,
				observe.StringAttr("app_id", s.appID),
				observe.StringAttr("echo_id", key.echoID),
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
	key := echoKey{appID: s.appID, echoID: selected.Run.EchoID}
	runErr := s.orchestrator.RunExisting(runContext, key.echoID, kernelecho.RunRequest{Message: selected.InputMessage}, func(event kernelecho.Event) error {
		s.hub.Publish(event)
		return nil
	})
	cancel()
	s.activeMu.Lock()
	delete(s.active, key)
	s.activeMu.Unlock()
	s.activeWG.Done()
	s.hub.Finish(s.appID, key.echoID)
	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, kernelecho.ErrInvalidTransition) &&
		!errors.Is(runErr, kernelecho.ErrRunRetryScheduled) {
		observe.Error(runContext, "持久调度 Run 执行失败", runErr)
	}
	s.signalWork()
	return true
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
	access.WriteJSON(writer, http.StatusOK, map[string]any{"echo": record, "runs": runs, "events": events})
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
	key := echoKey{appID: s.appID, echoID: echoID}
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
	cancel := s.active[key]
	s.activeMu.Unlock()
	if cancel == nil && !cancelledQueued {
		observe.Warn(request.Context(), "无法取消未运行的 Echo",
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "echo_not_running", "message": "Echo 当前不在运行"})
		return
	}
	if cancel != nil {
		cancel()
	}
	observe.Info(request.Context(), "已请求取消 Echo",
		observe.StringAttr("echo_id", echoID),
	)
	writer.WriteHeader(http.StatusNoContent)
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
