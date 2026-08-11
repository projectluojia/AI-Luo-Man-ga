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

func testMessage(appID, sessionID, messageID, platformMessageID string, createdAt time.Time) session.Message {
	return session.Message{
		AppID: appID, SessionID: sessionID, MessageID: messageID, SenderUserID: "user-1",
		Type:              session.MessageTypeText,
		ContentRef:        session.ContentRef{Mode: session.ContentModeInline, Size: int64(len("你好世界"))},
		PlatformMessageID: platformMessageID, CreatedAt: createdAt,
	}
}

func TestServicePersistsInlineAndBlobContent(t *testing.T) {
	service, store, _ := newTestService(t)
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()

	inlineBody := []byte("你好世界")
	inline, created, err := service.CreateMessage(context.Background(), testMessage("app-a", "session-1", "msg-inline", "", now), inlineBody)
	if err != nil || !created {
		t.Fatalf("内联消息写入 message=%#v created=%t err=%v", inline, created, err)
	}
	got, err := service.MessageContent(context.Background(), inline)
	if err != nil || !bytes.Equal(got, inlineBody) {
		t.Fatalf("内联正文读取=%q err=%v", got, err)
	}

	blobBody := []byte("图片二进制正文")
	blobMessage := testMessage("app-a", "session-1", "msg-blob", "", now)
	blobMessage.Type = session.MessageTypeImage
	blobMessage.ContentRef = session.ContentRef{Mode: session.ContentModeBlob, BlobID: "messages/msg-blob", Size: int64(len(blobBody))}
	stored, created, err := service.CreateMessage(context.Background(), blobMessage, blobBody)
	if err != nil || !created {
		t.Fatalf("Blob 消息写入 stored=%#v created=%t err=%v", stored, created, err)
	}
	got, err = service.MessageContent(context.Background(), stored)
	if err != nil || !bytes.Equal(got, blobBody) {
		t.Fatalf("Blob 正文读取=%q err=%v", got, err)
	}
}

func TestServicePlatformDedupProducesSingleMessage(t *testing.T) {
	service, store, _ := newTestService(t)
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()
	body := []byte("你好世界")

	first, created, err := service.CreateMessage(context.Background(), testMessage("app-a", "session-1", "msg-1", "platform-1", now), body)
	if err != nil || !created {
		t.Fatalf("首次投递 message=%#v created=%t err=%v", first, created, err)
	}
	replay, created, err := service.CreateMessage(context.Background(), testMessage("app-a", "session-1", "msg-1", "platform-1", now), body)
	if err != nil || created || replay.MessageID != "msg-1" {
		t.Fatalf("重复投递 replay=%#v created=%t err=%v", replay, created, err)
	}
	conflicting := testMessage("app-a", "session-1", "msg-2", "platform-1", now)
	if _, _, err := service.CreateMessage(context.Background(), conflicting, body); !errors.Is(err, session.ErrMessageConflict) {
		t.Fatalf("平台标识被不同消息复用 error=%v, want ErrMessageConflict", err)
	}
	history, err := store.ListMessages(context.Background(), "app-a", "session-1", session.MessageQuery{Limit: 10})
	if err != nil || len(history) != 1 {
		t.Fatalf("历史消息=%#v err=%v，应为 1 条", history, err)
	}
}

func TestServiceEditThenRecallHidesContent(t *testing.T) {
	service, store, _ := newTestService(t)
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()

	message, created, err := service.CreateMessage(context.Background(), testMessage("app-a", "session-1", "msg-1", "", now), []byte("你好世界"))
	if err != nil || !created {
		t.Fatalf("写入消息 message=%#v created=%t err=%v", message, created, err)
	}
	newBody := []byte("编辑后的正文")
	edit := session.MessageEdit{
		AppID: "app-a", SessionID: "session-1", MessageID: "msg-1",
		NewContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len(newBody))},
		EditedAt:      now.Add(time.Minute),
	}
	if err := service.EditMessage(context.Background(), edit, newBody); err != nil {
		t.Fatalf("编辑消息：%v", err)
	}
	edited, err := store.GetMessage(context.Background(), "app-a", "session-1", "msg-1")
	if err != nil || edited.EditedAt == nil || edited.ContentRef.Size != int64(len(newBody)) {
		t.Fatalf("编辑未生效 edited=%#v err=%v", edited, err)
	}
	got, err := service.MessageContent(context.Background(), edited)
	if err != nil || !bytes.Equal(got, newBody) {
		t.Fatalf("编辑后正文=%q err=%v", got, err)
	}

	// 撤回后：正文不可再装配、编辑与二次撤回均被拒绝。
	if err := store.RecallMessage(context.Background(), "app-a", "session-1", "msg-1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("撤回消息：%v", err)
	}
	if _, err := service.MessageContent(context.Background(), edited); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("撤回后正文仍可读取 error=%v", err)
	}
	if err := service.EditMessage(context.Background(), edit, newBody); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatalf("撤回后编辑 error=%v, want ErrInvalidTransition", err)
	}
	if err := store.RecallMessage(context.Background(), "app-a", "session-1", "msg-1", now.Add(3*time.Minute)); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatalf("二次撤回 error=%v, want ErrInvalidTransition", err)
	}
	// 撤回后的相同重复投递仍是同一条消息：回放返回既有消息，不产生新消息。
	replayMessage := testMessage("app-a", "session-1", "msg-1", "", now)
	replayMessage.ContentRef.Size = int64(len(newBody))
	replay, created, err := service.CreateMessage(context.Background(), replayMessage, newBody)
	if err != nil || created || replay.MessageID != "msg-1" {
		t.Fatalf("撤回后重复投递 replay=%#v created=%t err=%v", replay, created, err)
	}
	// 相同消息标识携带不同正文则是冲突。
	conflict := testMessage("app-a", "session-1", "msg-1", "", now.Add(time.Hour))
	conflict.ContentRef.Size = int64(len("完全不同的正文"))
	if _, _, err := service.CreateMessage(context.Background(), conflict, []byte("完全不同的正文")); !errors.Is(err, session.ErrMessageConflict) {
		t.Fatalf("撤回后同标识异内容 error=%v, want ErrMessageConflict", err)
	}
}

func TestServiceRejectsContentSizeMismatch(t *testing.T) {
	service, store, blobs := newTestService(t)
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()
	message := testMessage("app-a", "session-1", "msg-1", "", now)
	message.ContentRef.Size = 100 // 与实际正文长度不一致
	_, _, err := service.CreateMessage(context.Background(), message, []byte("短正文"))
	if !errors.Is(err, session.ErrInvalidMessage) {
		t.Fatalf("大小不匹配 error=%v, want ErrInvalidMessage", err)
	}
	if _, err := blobs.Size(context.Background(), "messages/msg-1"); !errors.Is(err, session.ErrBlobNotFound) {
		t.Fatalf("大小不匹配不应留下 Blob，得到 %v", err)
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
