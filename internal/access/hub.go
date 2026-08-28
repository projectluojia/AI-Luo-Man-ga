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
	"crypto/sha256"
	"encoding/json"
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
	MaxTextBytes = 4000 // 单条消息最大字符数（与 Web 适配器既有限制一致）
)

var (
	// ErrAppMismatch 消息 App 与 Hub 配置的 App 不一致。
	ErrAppMismatch = errors.New("inbound message app does not match hub app")
	// ErrIdentityRequired 用户消息缺少可解析的平台身份。
	ErrIdentityRequired = errors.New("authenticated platform identity is required")
	// ErrMembershipRequired 已绑定用户不是当前 App 的有效成员。
	ErrMembershipRequired = errors.New("app membership is required")
	// ErrIdentityContextInvalid 身份解析器返回了与请求边界不一致的上下文。
	ErrIdentityContextInvalid = errors.New("resolved identity context is invalid")
	// ErrHubConfiguration Hub 缺少身份解析器或会话存储等必要依赖。
	ErrHubConfiguration = errors.New("invalid access hub configuration")
)

// InboundMessage 是平台无关的标准入站消息。平台适配器负责把原始事件转换为
// 本标准消息；Hub 只消费本标准消息，不感知任何平台细节。
type InboundMessage struct {
	AppID             string
	Platform          string
	PlatformChannel   string // 平台渠道标识，当前闭集为 group/private
	PlatformSpaceID   string
	PlatformUserID    string // 平台侧不透明标识；用户消息必填
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
	EnsureSession(context.Context, session.Session) error
	CreateMessage(context.Context, session.Message, []byte) (session.Message, bool, error)
}

// IdentityResolver 是外部平台身份到内部用户的解析端口，由 identity.Service 实现。
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

// NewHub 构造统一入口。会话存储与身份解析器均为必需依赖，缺失时拒绝构造，
// 不提供匿名或临时身份降级路径。
func NewHub(appID string, sessions SessionStore, identities IdentityResolver) (*Hub, error) {
	if identity.ValidateAppID(appID) != nil || sessions == nil || identities == nil {
		return nil, ErrHubConfiguration
	}
	return &Hub{appID: appID, sessions: sessions, identities: identities}, nil
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
	sessionType := sessionTypeFor(message.PlatformChannel)
	sessionID := h.sessionIDFor(message, userID, sessionType)
	if err := h.ensureSession(ctx, message, sessionID, userID, sessionType); err != nil {
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

// ResolveIdentity 解析并校验当前 App 的外部身份，返回包含成员关系和生效权限
// 的受治理身份快照。它供非消息入口（例如 Capability invoke）复用同一身份边界。
func (h *Hub) ResolveIdentity(ctx context.Context, appID, platform, platformSpaceID, platformUserID string) (identity.IdentityContext, error) {
	if h == nil || appID != h.appID {
		return identity.IdentityContext{}, ErrAppMismatch
	}
	resolved, err := h.identities.ResolveIdentity(ctx, appID, platform, platformSpaceID, platformUserID)
	if err != nil {
		return identity.IdentityContext{}, fmt.Errorf("resolve platform identity: %w", err)
	}
	if err := ValidateIdentityContext(h.appID, resolved); err != nil {
		return identity.IdentityContext{}, err
	}
	return resolved, nil
}

// ValidateIdentityContext 校验身份解析器返回的 App 边界与成员关系。
// 非消息入口必须复用该校验，不能把外部平台标识直接当作内部用户。
func ValidateIdentityContext(appID string, resolved identity.IdentityContext) error {
	if resolved.AppID != appID || !session.ValidStableID(resolved.UserID) {
		return ErrIdentityContextInvalid
	}
	if resolved.Membership == nil {
		return ErrMembershipRequired
	}
	if resolved.Membership.AppID != appID || resolved.Membership.UserID != resolved.UserID {
		return ErrIdentityContextInvalid
	}
	return nil
}

func (h *Hub) validate(message InboundMessage) error {
	if message.AppID != h.appID {
		return fmt.Errorf("%w: app_id=%q", ErrAppMismatch, message.AppID)
	}
	if message.PlatformUserID == "" {
		return ErrIdentityRequired
	}
	if message.Platform == "" || message.PlatformSpaceID == "" || message.PlatformSessionID == "" ||
		message.PlatformMessageID == "" || message.IdempotencyKey == "" {
		return fmt.Errorf("%w: inbound message is missing platform identity fields", session.ErrInvalidMessage)
	}
	if _, _, _, _, err := identity.NormalizeBindingKey(message.AppID, message.Platform, message.PlatformSpaceID, message.PlatformUserID); err != nil {
		return err
	}
	if err := identity.ValidatePlatformUserID(message.PlatformSessionID); err != nil {
		return err
	}
	if sessionTypeFor(message.PlatformChannel) == "" {
		return fmt.Errorf("%w: inbound message has unsupported platform channel", session.ErrInvalidMessage)
	}
	if message.MessageType == "" {
		return fmt.Errorf("%w: inbound message is missing message type", session.ErrInvalidMessage)
	}
	if utf8.RuneCountInString(message.Text) == 0 || utf8.RuneCountInString(message.Text) > MaxTextBytes {
		return fmt.Errorf("%w: inbound message text exceeds %d characters", session.ErrInvalidMessage, MaxTextBytes)
	}
	return nil
}

func (h *Hub) resolveUser(ctx context.Context, message InboundMessage) (string, error) {
	resolved, err := h.ResolveIdentity(ctx, message.AppID, message.Platform, message.PlatformSpaceID, message.PlatformUserID)
	if err != nil {
		return "", err
	}
	return resolved.UserID, nil
}

func sessionTypeFor(channel string) string {
	switch channel {
	case "private":
		return session.SessionTypeDirect
	case "group":
		return session.SessionTypeGroup
	default:
		return ""
	}
}

// sessionIDFor 使用版本化规范结构派生固定长度会话标识。群会话不包含单个
// 发送者，因此同一群成员共享群历史；私聊包含内部用户，跨用户与跨渠道隔离。
func (h *Hub) sessionIDFor(message InboundMessage, userID, sessionType string) string {
	key := struct {
		Version           int    `json:"version"`
		AppID             string `json:"app_id"`
		Platform          string `json:"platform"`
		PlatformChannel   string `json:"platform_channel"`
		SessionType       string `json:"session_type"`
		PlatformSpaceID   string `json:"platform_space_id"`
		PlatformSessionID string `json:"platform_session_id"`
		UserID            string `json:"user_id,omitempty"`
	}{
		Version: 1, AppID: h.appID, Platform: message.Platform, PlatformChannel: message.PlatformChannel, SessionType: sessionType,
		PlatformSpaceID: message.PlatformSpaceID, PlatformSessionID: message.PlatformSessionID,
	}
	if sessionType == session.SessionTypeDirect {
		key.UserID = userID
	}
	encoded, _ := json.Marshal(key)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("session-v1-%x", digest[:])
}

func (h *Hub) ensureSession(ctx context.Context, message InboundMessage, sessionID, userID, sessionType string) error {
	now := message.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	role := session.MemberRoleOwner
	if sessionType == session.SessionTypeGroup {
		role = session.MemberRoleMember
	}
	created := session.Session{
		AppID:     h.appID,
		SessionID: sessionID,
		Type:      sessionType,
		Members: []session.Member{{
			UserID:   userID,
			Role:     role,
			JoinedAt: now,
		}},
		PlatformBindings: []session.PlatformBinding{{
			Platform:   message.Platform,
			PlatformID: sessionID,
			BoundAt:    now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.sessions.EnsureSession(ctx, created); err != nil {
		return fmt.Errorf("ensure session: %w", err)
	}
	return nil
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
