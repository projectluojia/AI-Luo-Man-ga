package session

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrSessionExists 会话已存在且与待创建会话内容冲突。
	ErrSessionExists = errors.New("session already exists")
	// ErrMessageConflict 消息标识或平台消息标识被不同内容的消息复用。
	ErrMessageConflict = errors.New("message identity conflict")
	// ErrMessageContentBlob 消息正文以 Blob 模式存放，须按 ContentRef.BlobID 从 BlobStore 读取。
	ErrMessageContentBlob = errors.New("message content is stored in the blob store")
	// ErrBlobNotFound Blob 不存在（或已被删除）。
	ErrBlobNotFound = errors.New("blob not found")
	// ErrBlobTooLarge Blob 内容超过存储大小上限。
	ErrBlobTooLarge = errors.New("blob content exceeds the storage limit")
)

// Store 是 Go 内核托管会话、消息与附件持久化的窄端口。
//
// 契约：
//   - 所有读写按 AppID 隔离，历史查询同时约束 AppID 与 SessionID；
//   - 平台消息按 app_id+platform_message_id 去重，同一平台消息重复投递只产生一条标准消息；
//   - 撤回是软删除（deleted_at），撤回后历史查询与正文读取均不可见；
//   - 消息正文只进入正文存储，绝不进入日志或审计载荷。
type Store interface {
	// CreateSession 创建会话；重复创建完全相同的会话为幂等成功，内容冲突返回 ErrSessionExists。
	CreateSession(context.Context, Session) error
	// GetSession 读取 App 内会话及其成员与平台绑定。
	GetSession(context.Context, string, string) (Session, error)

	// CreateMessage 幂等持久化一条标准消息，返回实际生效的消息与是否新建。
	// 相同 app_id+platform_message_id 的重复投递返回既有消息且 created 为 false；
	// 相同去重键携带不同内容返回 ErrMessageConflict。content 仅用于内联模式，
	// Blob 模式正文由 BlobStore 持有，此处只持久化引用。
	CreateMessage(context.Context, Message, []byte) (Message, bool, error)
	// GetMessage 读取 App 会话内未删除消息的元数据（不含正文）。
	GetMessage(context.Context, string, string, string) (Message, error)
	// ListMessages 按 App 与 Session 约束的历史查询，排除已删除消息。
	ListMessages(context.Context, string, string, MessageQuery) ([]Message, error)
	// GetMessageContent 读取内联模式正文（供上下文装配）；已删除消息返回
	// ErrMessageNotFound，Blob 模式消息返回 ErrMessageContentBlob。
	GetMessageContent(context.Context, string, string, string) ([]byte, error)
	// EditMessage 编辑未删除消息的正文引用并写入 edited_at；删除后编辑返回
	// ErrInvalidTransition，不存在返回 ErrMessageNotFound。
	EditMessage(context.Context, MessageEdit, []byte) error
	// RecallMessage 撤回消息（软删除），写入 deleted_at 后进入终态。
	RecallMessage(context.Context, string, string, string, time.Time) error

	// CreateAttachment 创建附件元数据；引用不存在的消息或会话返回相应错误。
	CreateAttachment(context.Context, Attachment) error
	// GetAttachment 读取 App 会话内的附件元数据。
	GetAttachment(context.Context, string, string, string) (Attachment, error)
}

// BlobStore 是消息正文与附件内容的窄端口。正文内容绝不进入日志或审计载荷。
type BlobStore interface {
	// Put 写入 Blob 内容；超出大小上限返回 ErrBlobTooLarge，非法标识返回 ErrInvalidBlobRef。
	Put(context.Context, string, []byte) error
	// Get 读取 Blob 内容；不存在或已删除返回 ErrBlobNotFound。
	Get(context.Context, string) ([]byte, error)
	// Delete 删除 Blob；删除后不可再读取。
	Delete(context.Context, string) error
	// Size 返回 Blob 内容字节数。
	Size(context.Context, string) (int64, error)
}
