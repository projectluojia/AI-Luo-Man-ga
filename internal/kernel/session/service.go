package session

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Service 是「会话、消息与附件」模块的最小服务逻辑：组合 Store 与 BlobStore，
// 提供幂等持久的消息写入原语与正文装配原语。与 Echo 创建的原子或 Outbox
// 组合留给上层编排，本服务不持有任何业务组合状态。
type Service struct {
	store Store
	blobs BlobStore
}

// NewService 构造消息写入服务。store 与 blobs 均不可为空。
func NewService(store Store, blobs BlobStore) (*Service, error) {
	if store == nil || blobs == nil {
		return nil, fmt.Errorf("session service requires both a Store and a BlobStore")
	}
	return &Service{store: store, blobs: blobs}, nil
}

// CreateMessage 持久化一条标准消息。内联模式把正文交给 Store；Blob 模式先写入
// BlobStore 再持久化元数据。相同平台消息的重复投递返回既有消息与 created=false。
func (s *Service) CreateMessage(ctx context.Context, message Message, content []byte) (stored Message, created bool, resultErr error) {
	started := time.Now()
	defer func() {
		if resultErr == nil {
			observe.Debug(ctx, "标准消息已经持久化",
				observe.StringAttr("app_id", message.AppID),
				observe.StringAttr("session_id", message.SessionID),
				observe.StringAttr("message_id", message.MessageID),
				observe.StringAttr("content_mode", message.ContentRef.Mode),
				observe.Int64Attr("content_size", message.ContentRef.Size),
				observe.BoolAttr("created", created),
				observe.Duration(started),
			)
		}
	}()
	if err := ValidateMessage(message); err != nil {
		return Message{}, false, err
	}
	if err := validateWriteContent(message.ContentRef, content); err != nil {
		return Message{}, false, err
	}
	if message.ContentRef.Mode == ContentModeBlob {
		if err := s.blobs.Put(ctx, message.ContentRef.BlobID, content); err != nil {
			return Message{}, false, err
		}
	}
	return s.store.CreateMessage(ctx, message, inlineContent(message.ContentRef, content))
}

// EditMessage 编辑未删除消息的正文引用。Blob 模式先写入 BlobStore 再更新元数据。
func (s *Service) EditMessage(ctx context.Context, edit MessageEdit, content []byte) (resultErr error) {
	started := time.Now()
	defer func() {
		if resultErr == nil {
			observe.Debug(ctx, "标准消息编辑已经生效",
				observe.StringAttr("app_id", edit.AppID),
				observe.StringAttr("session_id", edit.SessionID),
				observe.StringAttr("message_id", edit.MessageID),
				observe.StringAttr("content_mode", edit.NewContentRef.Mode),
				observe.Int64Attr("content_size", edit.NewContentRef.Size),
				observe.Duration(started),
			)
		}
	}()
	if err := ValidateMessageEdit(edit); err != nil {
		return err
	}
	if err := validateWriteContent(edit.NewContentRef, content); err != nil {
		return err
	}
	if edit.NewContentRef.Mode == ContentModeBlob {
		if err := s.blobs.Put(ctx, edit.NewContentRef.BlobID, content); err != nil {
			return err
		}
	}
	return s.store.EditMessage(ctx, edit, inlineContent(edit.NewContentRef, content))
}

// MessageContent 按消息的 ContentRef 装配正文（上下文装配原语）。
// 已删除消息不可读取正文；Blob 模式返回内容与声明的 Size 不一致时拒绝。
func (s *Service) MessageContent(ctx context.Context, message Message) ([]byte, error) {
	switch message.ContentRef.Mode {
	case ContentModeInline:
		return s.store.GetMessageContent(ctx, message.AppID, message.SessionID, message.MessageID)
	case ContentModeBlob:
		content, err := s.blobs.Get(ctx, message.ContentRef.BlobID)
		if err != nil {
			return nil, err
		}
		if int64(len(content)) != message.ContentRef.Size {
			return nil, ErrInvalidBlobRef
		}
		return content, nil
	default:
		return nil, ErrInvalidMessage
	}
}

// validateWriteContent 校验待写入正文与 ContentRef 声明的字节数一致。
func validateWriteContent(ref ContentRef, content []byte) error {
	if err := ValidateContentRef(ref); err != nil {
		return err
	}
	if int64(len(content)) != ref.Size {
		return ErrInvalidMessage
	}
	return nil
}

// inlineContent 内联模式返回正文本身，Blob 模式返回 nil（正文由 BlobStore 持有）。
func inlineContent(ref ContentRef, content []byte) []byte {
	if ref.Mode == ContentModeInline {
		return content
	}
	return nil
}
