// Package session 提供多入口连续对话的统一、持久、可审计数据模型。
//
// 本包是「会话、消息与附件」模块的内核侧契约：
//   - Session：会话生命周期与成员关系，必须归属某个 App；
//   - Message：标准消息，携带引用正文的 content_ref，支持回复、编辑、撤回与平台映射；
//   - Attachment / AttachmentRef：附件元数据与 Blob 引用；
//   - Store：Go 内核托管持久化的窄端口；
//   - BlobStore：正文/附件内容存储的窄端口。
//
// 平台来源不是会话类型：QQ 群、Web 群聊等都可以映射为 group 会话。
// 消息正文永不进入普通日志或审计载荷，本包只记录元数据与 Blob 引用。
package session

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SessionTypeDirect 私聊会话。
	SessionTypeDirect = "direct"
	// SessionTypeGroup 群聊会话（QQ 群、Web 群聊等平台均可映射为 group）。
	SessionTypeGroup = "group"
	// SessionTypeSystem 系统会话。
	SessionTypeSystem = "system"

	// MessageTypeText 文本消息。
	MessageTypeText = "text"
	// MessageTypeImage 图片消息。
	MessageTypeImage = "image"
	// MessageTypeFile 文件消息。
	MessageTypeFile = "file"
	// MessageTypeSystem 系统消息。
	MessageTypeSystem = "system"
	// MessageTypeEvent 事件消息。
	MessageTypeEvent = "event"

	// ContentModeInline 正文存于 messages.content 列。
	ContentModeInline = "inline"
	// ContentModeBlob 正文存于 BlobStore，messages 只保存 Blob 引用。
	ContentModeBlob = "blob"

	// MemberRoleOwner 会话所有者。
	MemberRoleOwner = "owner"
	// MemberRoleAdmin 会话管理员。
	MemberRoleAdmin = "admin"
	// MemberRoleMember 普通成员。
	MemberRoleMember = "member"

	// MaxInlineContentBytes 内联正文的最大字节数。
	MaxInlineContentBytes = 256 << 10
	// MaxMessageContentBytes 消息正文（含 Blob 模式）的最大字节数。
	MaxMessageContentBytes = 16 << 20
	// MaxAttachmentBytes 单个附件内容的最大字节数。
	MaxAttachmentBytes = 16 << 20
	// MaxMembersPerSession 单个会话的成员数量上限。
	MaxMembersPerSession = 1024
	// MaxBindingsPerSession 单个会话的平台映射数量上限。
	MaxBindingsPerSession = 32
	// MaxHistoryQueryLimit 历史查询的单次上限。
	MaxHistoryQueryLimit = 1000
	// MaxBlobIDLength Blob 标识的最大长度。
	MaxBlobIDLength = 256
	// MaxPlatformMessageIDLength 平台消息标识的最大长度。
	MaxPlatformMessageIDLength = 256
)

var (
	// ErrInvalidSession 会话校验失败。
	ErrInvalidSession = errors.New("invalid session")
	// ErrSessionNotFound 会话不存在。
	ErrSessionNotFound = errors.New("session not found")
	// ErrInvalidMessage 消息校验失败。
	ErrInvalidMessage = errors.New("invalid message")
	// ErrMessageNotFound 消息不存在（或已删除）。
	ErrMessageNotFound = errors.New("message not found")
	// ErrInvalidAttachment 附件校验失败。
	ErrInvalidAttachment = errors.New("invalid attachment")
	// ErrAttachmentNotFound 附件不存在。
	ErrAttachmentNotFound = errors.New("attachment not found")
	// ErrInvalidTransition 消息状态转换非法（编辑/撤回的重复或终态写入）。
	ErrInvalidTransition = errors.New("invalid message state transition")
	// ErrInvalidBlobRef Blob 引用非法。
	ErrInvalidBlobRef = errors.New("invalid blob reference")
)

var (
	stableIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	platformPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	blobIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	mimeTypePattern = regexp.MustCompile(`^[A-Za-z0-9.+-]+/[A-Za-z0-9.+-]+$`)
)

// Member 是会话的成员关系。
type Member struct {
	UserID   string    `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// PlatformBinding 是会话在某外部平台的映射。
type PlatformBinding struct {
	Platform   string    `json:"platform"`
	PlatformID string    `json:"platform_id"`
	BoundAt    time.Time `json:"bound_at"`
}

// Session 是 App 内的一段连续会话及其成员关系。
type Session struct {
	AppID            string
	SessionID        string
	Type             string
	Members          []Member
	PlatformBindings []PlatformBinding
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ContentRef 引用一条消息的正文。正文可以存于 messages.content 列（inline），
// 也可以存于 BlobStore（blob，messages 只保存 Blob 引用）。
type ContentRef struct {
	Mode   string `json:"mode"`
	BlobID string `json:"blob_id,omitempty"`
	Size   int64  `json:"size"`
}

// Message 是标准消息。MessageID 是稳定标识；平台来源与平台消息 ID 记录在
// PlatformMessageID 中用于按 app_id+platform_message_id 去重。
type Message struct {
	AppID             string
	SessionID         string
	MessageID         string
	SenderUserID      string
	Type              string
	ContentRef        ContentRef
	ReplyTo           string
	PlatformMessageID string
	CreatedAt         time.Time
	EditedAt          *time.Time
	DeletedAt         *time.Time
}

// AttachmentRef 是附件的 Blob 引用与展示元数据，不携带正文。
type AttachmentRef struct {
	Filename string
	MimeType string
	Size     int64
	BlobID   string
}

// Attachment 是附件实体：元数据落在 Go 托管存储，正文存于 BlobStore。
type Attachment struct {
	AppID          string
	AttachmentID   string
	SessionID      string
	MessageID      string
	UploaderUserID string
	Ref            AttachmentRef
	CreatedAt      time.Time
}

// ValidStableID 校验稳定标识（字母数字开头，允许 . _ : -）。
func ValidStableID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && stableIDPattern.MatchString(value)
}

// ValidBlobID 校验 Blob 标识：允许 '/' 作为命名空间分隔（如 "messages/msg-1"），
// 每个段必须是合法稳定标识，禁止空段、"." 与 ".." 段。
func ValidBlobID(value string) bool {
	if value == "" || len(value) > MaxBlobIDLength || !utf8.ValidString(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." ||
			!blobIDPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

// ValidPlatformMessageID 校验平台消息标识：空值允许（表示无平台来源，不去重）。
func ValidPlatformMessageID(value string) bool {
	if value == "" {
		return true
	}
	return len(value) <= MaxPlatformMessageIDLength && utf8.ValidString(value) && blobIDPattern.MatchString(value)
}

// ValidateContentRef 校验消息正文引用。
func ValidateContentRef(ref ContentRef) error {
	switch ref.Mode {
	case ContentModeInline:
		if ref.BlobID != "" || ref.Size <= 0 || ref.Size > MaxInlineContentBytes {
			return ErrInvalidMessage
		}
	case ContentModeBlob:
		if !ValidBlobID(ref.BlobID) || ref.Size <= 0 || ref.Size > MaxMessageContentBytes {
			return ErrInvalidMessage
		}
	default:
		return ErrInvalidMessage
	}
	return nil
}

// ValidateSession 校验会话的完整性与 App 边界约束。
func ValidateSession(session Session) error {
	if !ValidStableID(session.AppID) || !ValidStableID(session.SessionID) {
		return ErrInvalidSession
	}
	switch session.Type {
	case SessionTypeDirect, SessionTypeGroup, SessionTypeSystem:
	default:
		return ErrInvalidSession
	}
	if len(session.Members) > MaxMembersPerSession || len(session.PlatformBindings) > MaxBindingsPerSession {
		return ErrInvalidSession
	}
	if session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() || session.UpdatedAt.Before(session.CreatedAt) {
		return ErrInvalidSession
	}
	members := make(map[string]struct{}, len(session.Members))
	for _, member := range session.Members {
		if !ValidStableID(member.UserID) || member.JoinedAt.IsZero() {
			return ErrInvalidSession
		}
		switch member.Role {
		case MemberRoleOwner, MemberRoleAdmin, MemberRoleMember:
		default:
			return ErrInvalidSession
		}
		if _, duplicate := members[member.UserID]; duplicate {
			return ErrInvalidSession
		}
		members[member.UserID] = struct{}{}
	}
	bindings := make(map[string]struct{}, len(session.PlatformBindings))
	for _, binding := range session.PlatformBindings {
		if len(binding.Platform) > 64 || !utf8.ValidString(binding.Platform) || !platformPattern.MatchString(binding.Platform) ||
			!ValidPlatformMessageID(binding.PlatformID) || binding.PlatformID == "" || binding.BoundAt.IsZero() {
			return ErrInvalidSession
		}
		key := binding.Platform + "\x1f" + binding.PlatformID
		if _, duplicate := bindings[key]; duplicate {
			return ErrInvalidSession
		}
		bindings[key] = struct{}{}
	}
	return nil
}

// ValidateMessage 校验标准消息。
func ValidateMessage(message Message) error {
	if !ValidStableID(message.AppID) || !ValidStableID(message.SessionID) ||
		!ValidStableID(message.MessageID) || !ValidStableID(message.SenderUserID) {
		return ErrInvalidMessage
	}
	switch message.Type {
	case MessageTypeText, MessageTypeImage, MessageTypeFile, MessageTypeSystem, MessageTypeEvent:
	default:
		return ErrInvalidMessage
	}
	if err := ValidateContentRef(message.ContentRef); err != nil {
		return ErrInvalidMessage
	}
	if message.ReplyTo != "" && !ValidStableID(message.ReplyTo) {
		return ErrInvalidMessage
	}
	if !ValidPlatformMessageID(message.PlatformMessageID) {
		return ErrInvalidMessage
	}
	if message.CreatedAt.IsZero() {
		return ErrInvalidMessage
	}
	return nil
}

// ValidateMessageEdit 校验消息编辑请求。
func ValidateMessageEdit(edit MessageEdit) error {
	if !ValidStableID(edit.AppID) || !ValidStableID(edit.SessionID) || !ValidStableID(edit.MessageID) {
		return ErrInvalidMessage
	}
	if err := ValidateContentRef(edit.NewContentRef); err != nil {
		return ErrInvalidMessage
	}
	if edit.EditedAt.IsZero() {
		return ErrInvalidMessage
	}
	return nil
}

// ValidateMessageQuery 校验受限历史查询。
func ValidateMessageQuery(query MessageQuery) error {
	if query.SenderUserID != "" && !ValidStableID(query.SenderUserID) {
		return ErrInvalidMessage
	}
	if query.Limit < 1 || query.Limit > MaxHistoryQueryLimit {
		return ErrInvalidMessage
	}
	return nil
}

// ValidateAttachment 校验附件元数据。
func ValidateAttachment(attachment Attachment) error {
	if !ValidStableID(attachment.AppID) || !ValidStableID(attachment.SessionID) ||
		!ValidStableID(attachment.AttachmentID) || !ValidStableID(attachment.UploaderUserID) {
		return ErrInvalidAttachment
	}
	if attachment.MessageID != "" && !ValidStableID(attachment.MessageID) {
		return ErrInvalidAttachment
	}
	if err := ValidateAttachmentRef(attachment.Ref); err != nil {
		return ErrInvalidAttachment
	}
	if attachment.CreatedAt.IsZero() {
		return ErrInvalidAttachment
	}
	return nil
}

// ValidateAttachmentRef 校验附件 Blob 引用与展示元数据。
func ValidateAttachmentRef(ref AttachmentRef) error {
	if ref.Size <= 0 || ref.Size > MaxAttachmentBytes || !ValidBlobID(ref.BlobID) {
		return ErrInvalidAttachment
	}
	if !validFilename(ref.Filename) {
		return ErrInvalidAttachment
	}
	if len(ref.MimeType) > 128 || !utf8.ValidString(ref.MimeType) || !mimeTypePattern.MatchString(ref.MimeType) {
		return ErrInvalidAttachment
	}
	return nil
}

// validFilename 拒绝空名、超长、不可打印字符与路径分隔符，防止文件名进入日志或路径。
func validFilename(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) ||
		value == "." || value == ".." ||
		strings.ContainsAny(value, "/\\\x00\r\n") {
		return false
	}
	return true
}

// MessageEdit 是一次消息编辑请求：携带新的正文引用与编辑时间。
// 编辑只允许作用于未删除的消息；以最后一次成功的编辑为准。
type MessageEdit struct {
	AppID         string
	SessionID     string
	MessageID     string
	NewContentRef ContentRef
	EditedAt      time.Time
}

// MessageQuery 是受限历史查询参数。历史查询始终同时约束 AppID 与 SessionID，
// 并且只返回未删除的消息。
type MessageQuery struct {
	// SenderUserID 可选：仅返回该发送者的消息。
	SenderUserID string
	// Limit 单次查询的最大返回条数，必须位于 [1, MaxHistoryQueryLimit]。
	Limit int
}
