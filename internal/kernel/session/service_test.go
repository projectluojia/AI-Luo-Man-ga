package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func newTestService(t *testing.T) (*session.Service, *sqlite.Store, *blob.Store) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "session-service.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	blobs, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	service, err := session.NewService(store, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, blobs
}

func seedSession(t *testing.T, store *sqlite.Store, appID, sessionID string) {
	t.Helper()
	now := time.Now().UTC()
	err := store.CreateSession(context.Background(), session.Session{
		AppID: appID, SessionID: sessionID, Type: session.SessionTypeGroup,
		Members:   []session.Member{{UserID: "user-1", Role: session.MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testMessage(appID, sessionID, messageID string, createdAt time.Time) session.Message {
	return session.Message{
		AppID: appID, SessionID: sessionID, MessageID: messageID, SenderUserID: "user-1",
		Type:       session.MessageTypeText,
		ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len("你好世界"))},
		CreatedAt:  createdAt,
	}
}

func TestServiceAssemblesInlineAndBlobContent(t *testing.T) {
	service, store, blobs := newTestService(t)
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()

	// 内联正文：Store 直写，Service 按 ContentRef 装配。
	inlineBody := []byte("你好世界")
	inline, created, err := store.CreateMessage(context.Background(), testMessage("app-a", "session-1", "msg-inline", now), inlineBody)
	if err != nil || !created {
		t.Fatalf("内联消息写入 message=%#v created=%t err=%v", inline, created, err)
	}
	got, err := service.MessageContent(context.Background(), inline)
	if err != nil || !bytes.Equal(got, inlineBody) {
		t.Fatalf("内联正文读取=%q err=%v", got, err)
	}

	// Blob 正文：BlobStore 直写 + Store 持久化引用，Service 按引用装配。
	blobBody := []byte("图片二进制正文")
	blobMessage := testMessage("app-a", "session-1", "msg-blob", now)
	blobMessage.Type = session.MessageTypeImage
	blobMessage.ContentRef = session.ContentRef{Mode: session.ContentModeBlob, BlobID: "messages/msg-blob", Size: int64(len(blobBody))}
	if err := blobs.Put(context.Background(), blobMessage.ContentRef.BlobID, blobBody); err != nil {
		t.Fatalf("Blob 正文写入：%v", err)
	}
	stored, created, err := store.CreateMessage(context.Background(), blobMessage, nil)
	if err != nil || !created {
		t.Fatalf("Blob 消息持久化 stored=%#v created=%t err=%v", stored, created, err)
	}
	got, err = service.MessageContent(context.Background(), stored)
	if err != nil || !bytes.Equal(got, blobBody) {
		t.Fatalf("Blob 正文读取=%q err=%v", got, err)
	}

	// 已删除消息不可装配正文。
	if err := store.RecallMessage(context.Background(), "app-a", "session-1", "msg-inline", now.Add(time.Minute)); err != nil {
		t.Fatalf("撤回消息：%v", err)
	}
	if _, err := service.MessageContent(context.Background(), inline); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("撤回后正文仍可读取 error=%v", err)
	}
}

// TestMessageBodyNeverAppearsInLogsOrAudit 是正文/附件正文不进普通日志与审计载荷的
// 负向测试：即使日志或审计载荷试图携带正文，集中清洗规则也把正文替换为 [已脱敏]。
func TestMessageBodyNeverAppearsInLogsOrAudit(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := observe.New(observe.Config{
		Service: "test", Environment: "test", Format: "json", Writer: buffer,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("私有消息正文-绝不允许出现在日志")
	logger.Log(context.Background(), slog.LevelInfo, "消息写入完成",
		slog.String("app_id", "app-a"),
		slog.String("session_id", "session-1"),
		slog.String("message_id", "msg-1"),
		slog.Int64("content_size", int64(len(body))),
		slog.String("message_body", string(body)),
		slog.String("attachment_content", string(body)),
	)
	output := buffer.String()
	if !strings.Contains(output, `"message_id":"msg-1"`) || !strings.Contains(output, `"content_size":`) {
		t.Fatalf("日志缺少元数据字段：%s", output)
	}
	if bytes.Contains(buffer.Bytes(), body) {
		t.Fatalf("消息正文泄漏到日志：%s", output)
	}
	if !strings.Contains(output, "[已脱敏]") {
		t.Fatalf("消息正文未被脱敏：%s", output)
	}

	// 审计载荷同样必须脱敏。
	audit := observe.SanitizeAuditJSON([]byte(`{"attachment_id":"att-1","attachment_content":"私密附件正文","message_body":"私密消息正文"}`), 4096)
	var decoded map[string]any
	if err := json.Unmarshal(audit, &decoded); err != nil {
		t.Fatalf("审计载荷不是合法 JSON：%s", audit)
	}
	if bytes.Contains(audit, []byte("私密附件正文")) || bytes.Contains(audit, []byte("私密消息正文")) {
		t.Fatalf("审计载荷泄漏正文：%s", audit)
	}
	if decoded["attachment_content"] != "[已脱敏]" || decoded["message_body"] != "[已脱敏]" {
		t.Fatalf("审计载荷脱敏错误：%s", audit)
	}
}
