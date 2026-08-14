package session

import (
	"context"
	"fmt"
)

// Service 是「会话、消息与附件」模块的正文装配服务：组合 Store 与 BlobStore，
// 提供受限历史读取与消息正文装配原语（上下文装配入口）。消息写入由 Hub 经
// Store 窄端口直接完成；附件正文写入待授权接入落地后由 Blob 写入原语承担。
type Service struct {
	store Store
	blobs BlobStore
}

// NewService 构造消息装配服务。store 与 blobs 均不可为空。
func NewService(store Store, blobs BlobStore) (*Service, error) {
	if store == nil || blobs == nil {
		return nil, fmt.Errorf("session service requires both a Store and a BlobStore")
	}
	return &Service{store: store, blobs: blobs}, nil
}

// ListMessages 按 App 与 Session 约束读取受限历史（供上下文装配读取最近消息）。
// 返回未删除消息的元数据；正文按消息的 ContentRef 通过 MessageContent 读取。
func (s *Service) ListMessages(ctx context.Context, appID, sessionID string, query MessageQuery) ([]Message, error) {
	return s.store.ListMessages(ctx, appID, sessionID, query)
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
