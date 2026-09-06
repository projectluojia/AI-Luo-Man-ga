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
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

//go:embed static/*
var staticFiles embed.FS

type EchoReader interface {
	kernelecho.Reader
	ListRuns(context.Context, string, string) ([]kernelecho.RunRecord, error)
	GetRun(context.Context, string, string) (kernelecho.RunRecord, error)
}

type HealthChecker interface {
	Ping(context.Context) error
}

type Server struct {
	*access.AdmissionGate
	admission        kernelecho.Admission
	canceller        kernelecho.Canceller
	reader           EchoReader
	health           HealthChecker
	registry         *registry.Registry
	dispatcher       *runtime.Dispatcher
	policy           runtime.AppPolicy
	appID            string
	platformHub      *access.Hub
	identityResolver access.IdentityResolver
	webAuthenticator WebAuthenticator
	qqAccessAdmin    QQAccessAdmin
	hub              *access.EventHub
}

func NewServer(
	admission kernelecho.Admission,
	reader EchoReader,
	health HealthChecker,
	reg *registry.Registry,
	policy runtime.AppPolicy,
	appID string,
	platformHub *access.Hub,
	canceller kernelecho.Canceller,
	events *access.EventHub,
	options ...ServerOption,
) *Server {
	if admission == nil || canceller == nil || events == nil {
		panic("web access dependencies are incomplete")
	}
	server := &Server{
		AdmissionGate: access.NewAdmissionGate(),
		admission:     admission,
		canceller:     canceller,
		reader:        reader,
		health:        health,
		registry:      reg,
		policy:        policy,
		appID:         appID,
		platformHub:   platformHub,
		hub:           events,
	}
	if platformHub != nil {
		server.identityResolver = platformHub
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
	mux.HandleFunc("POST /api/v1/capabilities/{capability_id}/invoke", s.invokeCapability)
	mux.HandleFunc("GET /api/v1/admin/qq-access", s.getQQAccess)
	mux.HandleFunc("PUT /api/v1/admin/qq-access", s.updateQQAccess)
	mux.HandleFunc("POST /api/v2/echoes", s.createEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}", s.getEcho)
	mux.HandleFunc("GET /api/v1/runs/{run_id}", s.getRun)
	mux.HandleFunc("DELETE /api/v1/echoes/{echo_id}", s.cancelEcho)
	mux.HandleFunc("GET /api/v1/echoes/{echo_id}/events", s.echoEvents)
	// 产品前端聊天契约（LuoYing-Frontend）：流式接口，经标准 Intake → Echo →
	// 事件翻译链路，不直接暴露内核事件类型。
	mux.HandleFunc("POST /chat/stream", s.chatStream)
	static, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return observe.HTTPMiddleware("web_access", access.SecurityHeaders(mux))
}

func (s *Server) healthz(writer http.ResponseWriter, request *http.Request) {
	access.WriteJSON(writer, http.StatusOK, map[string]string{"status": "live"})
}

func (s *Server) readyz(writer http.ResponseWriter, request *http.Request) {
	if !s.Accepting() {
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
	items := make([]capability.CapabilitySpec, 0)
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
	if !s.Begin() {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "shutting_down", "message": "服务正在关闭"})
		return
	}
	defer s.Done()
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
	var input kernelecho.RunRequest
	if !access.DecodeJSONBody(writer, request, &input, 64<<10) {
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
	echoID, created, err := s.admission.Create(request.Context(), input)
	if err != nil {
		access.WriteEchoError(writer, request, err)
		return
	}
	if !created {
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
	runID := request.PathValue("run_id")
	run, err := s.reader.GetRun(request.Context(), s.appID, runID)
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
	cancelled, err := s.canceller.Cancel(request.Context(), echoID)
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
	if !cancelled {
		observe.Warn(request.Context(), "无法取消未运行的 Echo",
			observe.StringAttr("echo_id", echoID),
		)
		access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "echo_not_running", "message": "Echo 当前不在运行"})
		return
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
