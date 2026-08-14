// Package access 提供平台接入层的统一消息契约与入口处理（Hub）。
//
// 设计边界：
//   - 平台（Web、QQ、CLI 及未来渠道）只通过适配器产生标准 InboundMessage，
//     不直接创建 Echo、不写消息库、不解析身份；
//   - Hub.Intake 是受治理的统一入口：校验消息 → 解析外部身份 → 找到或创建
//     会话 → 持久化标准消息；Echo 创建由上层（Web 服务器等）在 Intake 成功后
//     进行，两者通过同一幂等键衔接，重复投递不会产生重复消息或重复 Echo；
//   - 会话与消息持久化在 Go 托管存储（session.Store 端口），平台来源不是
//     会话类型：QQ 群、Web 群聊都映射为 group；
//   - 消息正文只进入消息存储，绝不进入普通日志或审计载荷。
package access

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// 标准消息与载荷的边界限制。
const (
	MaxTextBytes       = 4000            // 单条消息最大字符数（与 Web 适配器既有限制一致）
	AnonymousSenderID  = "anonymous"     // 无身份渠道的保留发送者标识（不是 User 记录）
	AnonymousSessionID = "web-anonymous" // 无身份 Web 演示的共享会话标识
)

var (
	// ErrAppMismatch 消息 App 与 Hub 配置的 App 不一致。
	ErrAppMismatch = errors.New("inbound message app does not match hub app")
	// ErrAnonymousOnly Hub 未配置身份解析器时，携带平台身份的消息一律拒绝。
	ErrAnonymousOnly = errors.New("platform identity is not resolvable in this hub")
)

// InboundMessage 是平台无关的标准入站消息。平台适配器负责把原始事件转换为
// 本标准消息；Hub 只消费本标准消息，不感知任何平台细节。
type InboundMessage struct {
	AppID             string
	Platform          string
	PlatformChannel   string // 平台渠道标识（QQ: group/private）；无渠道概念的平台留空
	PlatformSpaceID   string
	PlatformUserID    string // 平台侧不透明标识；空表示无身份渠道（匿名）
	PlatformMessageID string // 平台消息标识，按 app_id+platform_message_id 去重
	PlatformSessionID string // 平台侧会话标识
	MessageType       string // 文本/图片/文件/事件等
	Text              string
	ReplyTo           string
	OccurredAt        time.Time
	IdempotencyKey    string
}

// SessionStore 是 Hub 需要的会话/消息持久化窄端口，由 *sqlite.Store 实现。
type SessionStore interface {
	CreateSession(context.Context, session.Session) error
	CreateMessage(context.Context, session.Message, []byte) (session.Message, bool, error)
}

// IdentityResolver 是外部平台身份到内部用户的解析端口，由 identity.Service 实现。
// 为 nil 时 Hub 只接受匿名消息，携带平台身份的消息一律拒绝（fail-closed）。
type IdentityResolver interface {
	ResolveIdentity(context.Context, string, string, string, string) (identity.IdentityContext, error)
}

// IntakeResult 是一次标准消息入库的结果，供上层创建 Echo。
type IntakeResult struct {
	UserID    string
	SessionID string
	MessageID string
	Text      string
	Created   bool // 消息是否新建（false 表示平台重复投递去重命中）
}

// Hub 是平台接入层的统一入口：校验标准消息、解析身份、持久化会话与消息。
type Hub struct {
	appID      string
	sessions   SessionStore
	identities IdentityResolver
}

// NewHub 构造统一入口。identities 为 nil 表示匿名渠道专用（如当前无身份的
// Web 演示），携带平台身份的消息会以 ErrAnonymousOnly 拒绝。
func NewHub(appID string, sessions SessionStore, identities IdentityResolver) *Hub {
	return &Hub{appID: appID, sessions: sessions, identities: identities}
}

// Intake 处理一条标准消息：校验 → 身份解析 → 会话找到或创建 → 消息持久化。
// 返回的消息标识与文本供上层创建 Echo；重复投递（相同 app_id+platform_message_id）
// 返回既有消息且 Created 为 false。
func (h *Hub) Intake(ctx context.Context, message InboundMessage) (IntakeResult, error) {
	if err := h.validate(message); err != nil {
		return IntakeResult{}, err
	}
	userID, err := h.resolveUser(ctx, message)
	if err != nil {
		return IntakeResult{}, err
	}
	sessionID := h.sessionIDFor(message, userID)
	if err := h.ensureSession(ctx, message, sessionID, userID); err != nil {
		return IntakeResult{}, err
	}
	stored, created, err := h.sessions.CreateMessage(ctx, h.toMessage(message, sessionID, userID), []byte(message.Text))
	if err != nil {
		return IntakeResult{}, err
	}
	observe.Info(ctx, "平台标准消息已入库",
		observe.StringAttr("app_id", h.appID),
		observe.StringAttr("platform", message.Platform),
		observe.StringAttr("session_id", sessionID),
		observe.StringAttr("message_id", stored.MessageID),
		observe.StringAttr("sender_user_id", userID),
		observe.BoolAttr("created", created),
	)
	return IntakeResult{
		UserID:    userID,
		SessionID: sessionID,
		MessageID: stored.MessageID,
		Text:      message.Text,
		Created:   created,
	}, nil
}

func (h *Hub) validate(message InboundMessage) error {
	if message.AppID != h.appID {
		return fmt.Errorf("%w: app_id=%q", ErrAppMismatch, message.AppID)
	}
	if message.Platform == "" || message.PlatformMessageID == "" || message.IdempotencyKey == "" {
		return fmt.Errorf("%w: inbound message is missing platform identity fields", session.ErrInvalidMessage)
	}
	if message.MessageType == "" {
		return fmt.Errorf("%w: inbound message is missing message type", session.ErrInvalidMessage)
	}
	if utf8.RuneCountInString(message.Text) > MaxTextBytes {
		return fmt.Errorf("%w: inbound message text exceeds %d characters", session.ErrInvalidMessage, MaxTextBytes)
	}
	return nil
}

func (h *Hub) resolveUser(ctx context.Context, message InboundMessage) (string, error) {
	if message.PlatformUserID == "" {
		return AnonymousSenderID, nil
	}
	if h.identities == nil {
		return "", ErrAnonymousOnly
	}
	resolved, err := h.identities.ResolveIdentity(ctx, message.AppID, message.Platform, message.PlatformSpaceID, message.PlatformUserID)
	if err != nil {
		return "", fmt.Errorf("resolve platform identity: %w", err)
	}
	return resolved.UserID, nil
}

// sessionIDFor 为消息确定会话标识：有身份时按 (app, platform, user) 派生，
// 匿名时使用保留会话。平台来源不会改变会话类型。
func (h *Hub) sessionIDFor(message InboundMessage, userID string) string {
	if userID == AnonymousSenderID {
		return AnonymousSessionID
	}
	return "session-" + userID
}

func (h *Hub) ensureSession(ctx context.Context, message InboundMessage, sessionID, userID string) error {
	now := message.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	platformID := message.PlatformSessionID
	if platformID == "" {
		platformID = sessionID
	}
	created := session.Session{
		AppID:     h.appID,
		SessionID: sessionID,
		Type:      session.SessionTypeDirect,
		Members: []session.Member{{
			UserID:   userID,
			Role:     session.MemberRoleOwner,
			JoinedAt: now,
		}},
		PlatformBindings: []session.PlatformBinding{{
			Platform:   message.Platform,
			PlatformID: platformID,
			BoundAt:    now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := h.sessions.CreateSession(ctx, created)
	if err == nil {
		return nil
	}
	if errors.Is(err, session.ErrSessionExists) {
		return nil // 幂等：会话已存在
	}
	return fmt.Errorf("create session: %w", err)
}

func (h *Hub) toMessage(message InboundMessage, sessionID, userID string) session.Message {
	now := message.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return session.Message{
		AppID:             h.appID,
		SessionID:         sessionID,
		MessageID:         message.IdempotencyKey, // 幂等键本身是合法稳定标识，且与 Echo 幂等键一致
		SenderUserID:      userID,
		Type:              message.MessageType,
		ContentRef:        session.ContentRef{Mode: session.ContentModeInline, Size: int64(len([]byte(message.Text)))},
		ReplyTo:           message.ReplyTo,
		PlatformMessageID: message.PlatformMessageID,
		CreatedAt:         now,
	}
}
