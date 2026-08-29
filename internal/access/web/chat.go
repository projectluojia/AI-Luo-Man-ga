package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// chatRequest 是产品前端（LuoYing-Frontend）的聊天契约：文本消息经受治理
// Intake 进入平台标准链路。session_id 只用于平台会话绑定记录，客户端提供的
// user_id/user_name/session_id 不作为身份依据，身份只来自可信 Web 登录态。
type chatRequest struct {
	SessionID string   `json:"session_id"`
	UserID    string   `json:"user_id"`
	UserName  string   `json:"user_name"`
	Text      string   `json:"text"`
	ImageIDs  []string `json:"image_ids"`
	FileIDs   []string `json:"file_ids"`
}

// chatStream 提供前端流式聊天契约：POST /chat/stream。请求经标准 Intake →
// 幂等 Echo 创建 → 事件订阅，把内核 Echo 事件翻译为前端 SSE 协议
// （track/text_delta/final/error/done）。
func (s *Server) chatStream(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "streaming_unavailable"})
		return
	}
	webIdentity, authenticated := s.authenticateWeb(writer, request)
	if !authenticated {
		return
	}
	req, ok := s.decodeChatRequest(writer, request)
	if !ok {
		return
	}
	echoID, created, ok := s.startChatRun(writer, request, req, webIdentity)
	if !ok {
		return
	}
	live, unsubscribe := s.hub.Subscribe(s.appID, echoID)
	defer unsubscribe()
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	if created {
		s.queueEcho(request.Context(), echoID)
	}
	started := time.Now()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	finalSent := false
	for {
		select {
		case <-request.Context().Done():
			observe.Debug(request.Context(), "前端聊天流连接已经断开",
				observe.StringAttr("echo_id", echoID),
				observe.Duration(started),
			)
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-live:
			if !open {
				if !finalSent {
					// 流在没有终态回复的情况下关闭（取消或内部失败）：告知前端。
					_ = writeChatSSE(writer, "error", map[string]string{"error": "请求未能完成"})
					_ = writeChatSSE(writer, "done", map[string]any{})
				}
				return
			}
			terminal := s.translateChatEvent(writer, event)
			if terminal {
				finalSent = true
				_ = writeChatSSE(writer, "done", map[string]any{})
				return
			}
			flusher.Flush()
		}
	}
}

// decodeChatRequest 严格解码前端聊天请求体：未知字段拒绝、单 JSON 对象、
// 文本 1 至 4000 字符；附件字段暂不支持，非空即拒绝（fail-closed，不静默忽略）。
func (s *Server) decodeChatRequest(writer http.ResponseWriter, request *http.Request) (*chatRequest, bool) {
	if !s.beginAdmission() {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "shutting_down", "message": "服务正在关闭"})
		return nil, false
	}
	defer s.admissionWG.Done()
	var input chatRequest
	if !access.DecodeJSONBody(writer, request, &input, 64<<10) {
		return nil, false
	}
	input.Text = strings.TrimSpace(input.Text)
	if input.Text == "" || utf8.RuneCountInString(input.Text) > 4000 {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_message", "message": "消息长度必须为 1 至 4000 个字符"})
		return nil, false
	}
	if len(input.ImageIDs) > 0 || len(input.FileIDs) > 0 {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "attachments_unsupported", "message": "附件消息暂不支持"})
		return nil, false
	}
	return &input, true
}

// startChatRun 执行受治理的标准链路：平台消息入库 → 幂等 Echo 创建。
// 平台消息标识与幂等键由服务端生成（前端契约不携带幂等键）。
func (s *Server) startChatRun(writer http.ResponseWriter, request *http.Request, req *chatRequest, webIdentity AuthenticatedWebIdentity) (string, bool, bool) {
	idempotencyKey := uuid.NewString()
	intake, err := s.platformHub.Intake(request.Context(), access.InboundMessage{
		AppID:             s.appID,
		Platform:          "web",
		PlatformChannel:   "private",
		PlatformSpaceID:   webIdentity.PlatformSpaceID,
		PlatformUserID:    webIdentity.PlatformUserID,
		PlatformMessageID: idempotencyKey,
		PlatformSessionID: webIdentity.PlatformSessionID,
		MessageType:       "text",
		Text:              req.Text,
		OccurredAt:        time.Now().UTC(),
		IdempotencyKey:    idempotencyKey,
	})
	if err != nil {
		access.WriteIntakeError(writer, request, err)
		return "", false, false
	}
	input := kernelecho.RunRequest{
		Message:        intake.Text,
		IdempotencyKey: idempotencyKey,
		SessionID:      intake.SessionID,
		UserID:         intake.UserID,
		MessageID:      intake.MessageID,
		Channel:        "web",
	}
	echoID, created, err := s.orchestrator.CreateIdempotent(request.Context(), input)
	if err != nil {
		access.WriteEchoError(writer, request, err)
		return "", false, false
	}
	observe.Info(request.Context(), "前端聊天请求已进入标准链路",
		observe.StringAttr("app_id", s.appID),
		observe.StringAttr("echo_id", echoID),
		observe.BoolAttr("idempotency_replayed", !created),
	)
	return echoID, created, true
}

// translateChatEvent 把内核 Echo 事件翻译为前端流式事件并写出；返回值表示
// 事件是否为终态（reply.final / run.failed），终态后流必须结束。
func (s *Server) translateChatEvent(writer io.Writer, event kernelecho.Event) (terminal bool) {
	switch event.Type {
	case "echo.started":
		_ = writeChatSSE(writer, "track", map[string]any{
			"kind": "agent_action", "text": "已收到你的问题，正在处理",
		})
		return false
	case "capability.completed":
		var payload struct {
			CapabilityID string `json:"capability_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.CapabilityID == "" {
			return false
		}
		_ = writeChatSSE(writer, "track", map[string]any{
			"kind": "agent_action", "text": "已调用能力 " + payload.CapabilityID,
		})
		return false
	case "reply.delta":
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Text == "" {
			return false
		}
		_ = writeChatSSE(writer, "text_delta", map[string]string{"text": payload.Text})
		return false
	case "reply.final":
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Text == "" {
			return false
		}
		_ = writeChatSSE(writer, "final", map[string]string{"reply": payload.Text})
		return true
	case "run.failed":
		message := "处理失败"
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err == nil && payload.Message != "" {
			message = payload.Message
		}
		_ = writeChatSSE(writer, "error", map[string]string{"error": message})
		return true
	default:
		return false
	}
}

// writeChatSSE 按前端流式协议写出一个事件。
func writeChatSSE(writer io.Writer, eventName string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", eventName, encoded)
	return err
}
