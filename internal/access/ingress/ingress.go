// Package ingress 提供统一平台事件入口：任何平台适配器把原始事件规范化后
// POST 到内核，经 Hub 走身份解析 → 会话 → 消息 → Echo 创建的受治理链路。
// 平台不直接创建 Echo、不写消息库、不解析身份（与 Web 适配器同约束）。
package ingress

import (
	"net/http"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	// maxEventBytes 是单条平台事件的请求体上限。
	maxEventBytes = 64 << 10
)

// Event 是平台适配器推送的统一平台事件（规范化入站消息）。
// platform 由请求路径提供；用户身份与渠道类型均为必填，不提供匿名降级。
type Event struct {
	PlatformChannel   string    `json:"platform_channel"`
	PlatformSpaceID   string    `json:"platform_space_id"`
	PlatformUserID    string    `json:"platform_user_id"`
	PlatformSessionID string    `json:"platform_session_id"`
	PlatformMessageID string    `json:"platform_message_id"`
	MessageType       string    `json:"message_type"`
	Text              string    `json:"text"`
	ReplyTo           string    `json:"reply_to,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
	IdempotencyKey    string    `json:"idempotency_key"`
}

// Server 是平台事件入口：POST /api/v1/ingress/{platform}。
type Server struct {
	*access.AdmissionGate
	appID     string
	hub       *access.Hub
	echoes    kernelecho.Creator
	scheduler kernelecho.Enqueuer
}

// NewServer 构造平台事件入口。
func NewServer(appID string, hub *access.Hub, echoes kernelecho.Creator, scheduler kernelecho.Enqueuer) *Server {
	if scheduler == nil {
		panic("platform ingress requires a Run scheduler")
	}
	return &Server{AdmissionGate: access.NewAdmissionGate(), appID: appID, hub: hub, echoes: echoes, scheduler: scheduler}
}

// Handler 返回平台事件 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/ingress/{platform}", s.ingest)
	return observe.HTTPMiddleware("platform_ingress", access.SecurityHeaders(mux))
}

// ingest 处理一条平台事件：严格解码 → 标准消息校验（Hub）→ 身份解析 → 会话/消息入库 →
// Echo 创建。重复投递（相同幂等键）返回既有 Echo 且 created 为 false。
func (s *Server) ingest(writer http.ResponseWriter, request *http.Request) {
	if !s.Begin() {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "shutting_down", "message": "服务正在关闭"})
		return
	}
	defer s.Done()
	platform := request.PathValue("platform")
	if err := identity.ValidatePlatform(platform); err != nil {
		observe.Warn(request.Context(), "平台事件路径标识非法", observe.StringAttr("reason", platform))
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "平台标识不合法"})
		return
	}
	var event Event
	if !access.DecodeJSONBody(writer, request, &event, maxEventBytes) {
		return
	}
	intake, err := s.hub.Intake(request.Context(), s.toInbound(platform, event))
	if err != nil {
		access.WriteIntakeError(writer, request, err)
		return
	}
	echoID, created, err := s.echoes.CreateIdempotent(request.Context(), kernelecho.RunRequest{
		Message: intake.Text, IdempotencyKey: event.IdempotencyKey,
		SessionID: intake.SessionID, UserID: intake.UserID, MessageID: intake.MessageID,
	})
	if err != nil {
		access.WriteEchoError(writer, request, err)
		return
	}
	if created {
		s.scheduler.Enqueue(request.Context(), echoID)
	}
	observe.Info(request.Context(), "平台事件已完成 Echo 创建",
		observe.StringAttr("app_id", s.appID),
		observe.StringAttr("platform", platform),
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("session_id", intake.SessionID),
		observe.StringAttr("message_id", intake.MessageID),
		observe.StringAttr("sender_user_id", intake.UserID),
		observe.BoolAttr("created", created),
	)
	access.WriteJSON(writer, http.StatusOK, map[string]any{
		"echo_id": echoID, "session_id": intake.SessionID, "message_id": intake.MessageID,
		"sender_user_id": intake.UserID, "created": created,
	})
}

// toInbound 把规范化事件转换为 Hub 的标准入站消息。
func (s *Server) toInbound(platform string, event Event) access.InboundMessage {
	return access.InboundMessage{
		AppID:             s.appID,
		Platform:          platform,
		PlatformChannel:   event.PlatformChannel,
		PlatformSpaceID:   event.PlatformSpaceID,
		PlatformUserID:    event.PlatformUserID,
		PlatformMessageID: event.PlatformMessageID,
		PlatformSessionID: event.PlatformSessionID,
		MessageType:       event.MessageType,
		Text:              event.Text,
		ReplyTo:           event.ReplyTo,
		OccurredAt:        event.OccurredAt,
		IdempotencyKey:    event.IdempotencyKey,
	}
}
