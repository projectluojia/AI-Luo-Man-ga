package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func newSessionStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
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

func TestSessionCreationIsIdempotentAndAppScoped(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first := session.Session{
		AppID: "app-a", SessionID: "session-1", Type: session.SessionTypeGroup,
		Members:   []session.Member{{UserID: "user-1", Role: session.MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	// 完全相同的会话重复创建是幂等成功。
	if err := store.CreateSession(ctx, first); err != nil {
		t.Fatalf("幂等重放失败：%v", err)
	}
	// 相同标识不同内容冲突。
	conflicting := first
	conflicting.Members = append(conflicting.Members, session.Member{UserID: "user-2", Role: session.MemberRoleMember, JoinedAt: now})
	if err := store.CreateSession(ctx, conflicting); !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("冲突会话 error=%v, want ErrSessionExists", err)
	}
	// 其他 App 读取不到该会话。
	if _, err := store.GetSession(ctx, "app-b", "session-1"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("跨 App 读取 error=%v, want ErrSessionNotFound", err)
	}
	got, err := store.GetSession(ctx, "app-a", "session-1")
	if err != nil || got.Type != session.SessionTypeGroup || len(got.Members) != 1 || got.Members[0].UserID != "user-1" {
		t.Fatalf("读取会话=%#v err=%v", got, err)
	}
}

func TestEnsureSessionAtomicallyAddsMembersAndRejectsBindingConflicts(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := session.Session{
		AppID: "app-a", SessionID: "session-v1-group", Type: session.SessionTypeGroup,
		Members:          []session.Member{{UserID: "user-1", Role: session.MemberRoleMember, JoinedAt: now}},
		PlatformBindings: []session.PlatformBinding{{Platform: "qq", PlatformID: "binding-v1", BoundAt: now}},
		CreatedAt:        now, UpdatedAt: now,
	}
	if err := store.EnsureSession(ctx, base); err != nil {
		t.Fatal(err)
	}
	second := base
	second.Members = []session.Member{{UserID: "user-2", Role: session.MemberRoleMember, JoinedAt: now.Add(time.Second)}}
	second.UpdatedAt = now.Add(time.Second)
	if err := store.EnsureSession(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSession(ctx, second); err != nil {
		t.Fatalf("member replay failed: %v", err)
	}
	conflicting := base
	conflicting.Members = []session.Member{{UserID: "user-3", Role: session.MemberRoleMember, JoinedAt: now.Add(2 * time.Second)}}
	conflicting.PlatformBindings = []session.PlatformBinding{{Platform: "qq", PlatformID: "other-binding", BoundAt: now}}
	conflicting.UpdatedAt = now.Add(2 * time.Second)
	if err := store.EnsureSession(ctx, conflicting); !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("binding conflict error=%v, want ErrSessionExists", err)
	}
	stored, err := store.GetSession(ctx, "app-a", base.SessionID)
	if err != nil || len(stored.Members) != 2 {
		t.Fatalf("stored session=%#v err=%v", stored, err)
	}
	for _, member := range stored.Members {
		if member.UserID == "user-3" {
			t.Fatal("conflicting ensure partially inserted member")
		}
	}
}

func TestMessageReadsAndHistoryConstrainAppAndSession(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")
	seedSession(t, store, "app-a", "session-2")

	message := testMessage("app-a", "session-1", "msg-1", "platform-1", now)
	if _, created, err := store.CreateMessage(ctx, message, []byte("你好世界")); err != nil || !created {
		t.Fatalf("写入消息 created=%t err=%v", created, err)
	}
	if _, err := store.GetMessage(ctx, "app-b", "session-1", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("跨 App 读取 error=%v, want ErrMessageNotFound", err)
	}
	if _, err := store.GetMessage(ctx, "app-a", "session-2", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("跨 Session 读取 error=%v, want ErrMessageNotFound", err)
	}
	// 历史查询同时约束 app_id 与 session_id。
	history, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 10})
	if err != nil || len(history) != 1 || history[0].MessageID != "msg-1" {
		t.Fatalf("历史消息=%#v err=%v", history, err)
	}
	for _, appSession := range [][2]string{{"app-b", "session-1"}, {"app-a", "session-2"}} {
		history, err = store.ListMessages(ctx, appSession[0], appSession[1], session.MessageQuery{Limit: 10})
		if err != nil || len(history) != 0 {
			t.Fatalf("跨边界历史 %v=%#v err=%v", appSession, history, err)
		}
	}
	// 跨边界正文读取同样被拒绝。
	if _, err := store.GetMessageContent(ctx, "app-b", "session-1", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("跨 App 正文读取 error=%v", err)
	}
	if _, err := store.GetMessageContent(ctx, "app-a", "session-2", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("跨 Session 正文读取 error=%v", err)
	}
}

func TestPlatformMessageDedupIsIdempotentAndAppScoped(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")
	seedSession(t, store, "app-b", "session-1")

	message := testMessage("app-a", "session-1", "msg-1", "platform-1", now)
	_, created, err := store.CreateMessage(ctx, message, []byte("你好世界"))
	if err != nil || !created {
		t.Fatalf("首次投递 created=%t err=%v", created, err)
	}
	replay, created, err := store.CreateMessage(ctx, message, []byte("你好世界"))
	if err != nil || created || replay.MessageID != "msg-1" {
		t.Fatalf("重复投递 replay=%#v created=%t err=%v", replay, created, err)
	}
	// 同一平台标识复用为不同消息是冲突。
	other := testMessage("app-a", "session-1", "msg-2", "platform-1", now)
	if _, _, err := store.CreateMessage(ctx, other, []byte("你好世界")); !errors.Is(err, session.ErrMessageConflict) {
		t.Fatalf("平台标识冲突 error=%v, want ErrMessageConflict", err)
	}
	// 去重按 App 隔离：另一个 App 可以使用相同平台标识。
	otherApp := testMessage("app-b", "session-1", "msg-1", "platform-1", now)
	if _, created, err := store.CreateMessage(ctx, otherApp, []byte("你好世界")); err != nil || !created {
		t.Fatalf("跨 App 去重错误 created=%t err=%v", created, err)
	}
	// 不同消息标识但内容完全相同也各自成条。
	if _, created, err := store.CreateMessage(ctx, testMessage("app-a", "session-1", "msg-3", "", now), []byte("你好世界")); err != nil || !created {
		t.Fatalf("无平台标识消息 created=%t err=%v", created, err)
	}
	history, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 10})
	if err != nil || len(history) != 2 {
		t.Fatalf("app-a 历史消息=%#v err=%v，应为 2 条", history, err)
	}
}

func TestPlatformMessageDedupConcurrentDeliveryCreatesOneMessage(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")

	message := testMessage("app-a", "session-1", "msg-1", "platform-1", now)
	const workers = 8
	var wg sync.WaitGroup
	createdCount := 0
	var countMutex sync.Mutex
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.CreateMessage(ctx, message, []byte("你好世界"))
			if err != nil {
				t.Errorf("并发投递错误：%v", err)
				return
			}
			if created {
				countMutex.Lock()
				createdCount++
				countMutex.Unlock()
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatalf("并发重复投递产生了 %d 条新消息，应为 1", createdCount)
	}
	history, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 10})
	if err != nil || len(history) != 1 {
		t.Fatalf("并发去重后历史=%#v err=%v", history, err)
	}
}

func TestSoftDeletedMessageIsInvisibleToHistoryAndContext(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")
	seedSession(t, store, "app-a", "session-2")

	message := testMessage("app-a", "session-1", "msg-1", "", now)
	if _, created, err := store.CreateMessage(ctx, message, []byte("你好世界")); err != nil || !created {
		t.Fatalf("写入消息 created=%t err=%v", created, err)
	}
	if _, err := store.GetMessage(ctx, "app-a", "session-1", "msg-1"); err != nil {
		t.Fatalf("撤回前读取失败：%v", err)
	}
	if err := store.RecallMessage(ctx, "app-a", "session-1", "msg-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("撤回消息：%v", err)
	}
	// 查询与正文读取都不可见。
	if _, err := store.GetMessage(ctx, "app-a", "session-1", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("撤回后读取 error=%v, want ErrMessageNotFound", err)
	}
	if _, err := store.GetMessageContent(ctx, "app-a", "session-1", "msg-1"); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("撤回后正文读取 error=%v, want ErrMessageNotFound", err)
	}
	history, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 10})
	if err != nil || len(history) != 0 {
		t.Fatalf("撤回后历史=%#v err=%v，应排除已删除消息", history, err)
	}
	// 撤回是终态：编辑与二次撤回被拒绝。
	edit := session.MessageEdit{
		AppID: "app-a", SessionID: "session-1", MessageID: "msg-1",
		NewContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len("新正文"))},
		EditedAt:      now.Add(2 * time.Minute),
	}
	if err := store.EditMessage(ctx, edit, []byte("新正文")); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatalf("撤回后编辑 error=%v, want ErrInvalidTransition", err)
	}
	if err := store.RecallMessage(ctx, "app-a", "session-1", "msg-1", now.Add(3*time.Minute)); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatalf("二次撤回 error=%v, want ErrInvalidTransition", err)
	}
	// 其他会话的其他消息不受影响。
	if _, created, err := store.CreateMessage(ctx, testMessage("app-a", "session-2", "msg-2", "", now), []byte("你好世界")); err != nil || !created {
		t.Fatalf("其他会话写入 created=%t err=%v", created, err)
	}
	if _, err := store.GetMessage(ctx, "app-a", "session-2", "msg-2"); err != nil {
		t.Fatalf("其他会话消息读取失败：%v", err)
	}
}

func TestMessageEditStateTransitions(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")

	if _, created, err := store.CreateMessage(ctx, testMessage("app-a", "session-1", "msg-1", "", now), []byte("你好世界")); err != nil || !created {
		t.Fatalf("写入消息 created=%t err=%v", created, err)
	}
	// 编辑不存在消息。
	missing := session.MessageEdit{
		AppID: "app-a", SessionID: "session-1", MessageID: "missing",
		NewContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len("新正文"))},
		EditedAt:      now.Add(time.Minute),
	}
	if err := store.EditMessage(ctx, missing, []byte("新正文")); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("编辑不存在消息 error=%v, want ErrMessageNotFound", err)
	}
	// 合法编辑。
	edit := session.MessageEdit{
		AppID: "app-a", SessionID: "session-1", MessageID: "msg-1",
		NewContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len("编辑后的正文"))},
		EditedAt:      now.Add(time.Minute),
	}
	if err := store.EditMessage(ctx, edit, []byte("编辑后的正文")); err != nil {
		t.Fatalf("编辑消息：%v", err)
	}
	edited, err := store.GetMessage(ctx, "app-a", "session-1", "msg-1")
	if err != nil || edited.EditedAt == nil || edited.ContentRef.Size != int64(len("编辑后的正文")) {
		t.Fatalf("编辑未生效 edited=%#v err=%v", edited, err)
	}
	// 跨 Session 编辑失败且不产生副作用。
	crossSession := edit
	crossSession.SessionID = "session-2"
	if err := store.EditMessage(ctx, crossSession, []byte("编辑后的正文")); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("跨 Session 编辑 error=%v, want ErrMessageNotFound", err)
	}
	// 正文大小不匹配被拒绝。
	badSize := edit
	badSize.NewContentRef.Size = 999
	if err := store.EditMessage(ctx, badSize, []byte("编辑后的正文")); !errors.Is(err, session.ErrInvalidMessage) {
		t.Fatalf("编辑大小不匹配 error=%v, want ErrInvalidMessage", err)
	}
}

func TestMessageQueryValidatesLimitsAndSender(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	seedSession(t, store, "app-a", "session-1")
	now := time.Now().UTC()
	for index := 0; index < 3; index++ {
		message := testMessage("app-a", "session-1", "msg-"+string(rune('a'+index)), "", now.Add(time.Duration(index)*time.Second))
		if index == 0 {
			message.SenderUserID = "user-2"
		}
		if _, created, err := store.CreateMessage(ctx, message, []byte("你好世界")); err != nil || !created {
			t.Fatalf("写入消息 %d created=%t err=%v", index, created, err)
		}
	}
	// 发送者过滤。
	filtered, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 10, SenderUserID: "user-2"})
	if err != nil || len(filtered) != 1 || filtered[0].MessageID != "msg-a" {
		t.Fatalf("发送者过滤=%#v err=%v", filtered, err)
	}
	// 非法查询参数被拒绝。
	if _, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: 0}); !errors.Is(err, session.ErrInvalidMessage) {
		t.Fatalf("零 Limit error=%v, want ErrInvalidMessage", err)
	}
	if _, err := store.ListMessages(ctx, "app-a", "session-1", session.MessageQuery{Limit: session.MaxHistoryQueryLimit + 1}); !errors.Is(err, session.ErrInvalidMessage) {
		t.Fatalf("超限 Limit error=%v, want ErrInvalidMessage", err)
	}
}

func TestAttachmentScopingAndReferenceErrors(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")
	seedSession(t, store, "app-a", "session-2")
	if _, created, err := store.CreateMessage(ctx, testMessage("app-a", "session-1", "msg-1", "", now), []byte("你好世界")); err != nil || !created {
		t.Fatalf("写入消息 created=%t err=%v", created, err)
	}

	attachment := session.Attachment{
		AppID: "app-a", SessionID: "session-1", AttachmentID: "att-1",
		MessageID: "msg-1", UploaderUserID: "user-1",
		Ref:       session.AttachmentRef{Filename: "报告.pdf", MimeType: "application/pdf", Size: 100, BlobID: "attachments/att-1"},
		CreatedAt: now,
	}
	if err := store.CreateAttachment(ctx, attachment); err != nil {
		t.Fatalf("创建附件：%v", err)
	}
	// 引用不存在的消息。
	missingMessage := attachment
	missingMessage.AttachmentID = "att-2"
	missingMessage.MessageID = "missing-msg"
	missingMessage.Ref.BlobID = "attachments/att-2"
	if err := store.CreateAttachment(ctx, missingMessage); !errors.Is(err, session.ErrMessageNotFound) {
		t.Fatalf("附件引用不存在消息 error=%v, want ErrMessageNotFound", err)
	}
	// 引用不存在的会话。
	missingSession := attachment
	missingSession.AttachmentID = "att-3"
	missingSession.SessionID = "session-3"
	missingSession.Ref.BlobID = "attachments/att-3"
	if err := store.CreateAttachment(ctx, missingSession); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("附件引用不存在会话 error=%v, want ErrSessionNotFound", err)
	}
	// App 与 Session 隔离读取。
	if _, err := store.GetAttachment(ctx, "app-b", "session-1", "att-1"); !errors.Is(err, session.ErrAttachmentNotFound) {
		t.Fatalf("跨 App 附件读取 error=%v, want ErrAttachmentNotFound", err)
	}
	if _, err := store.GetAttachment(ctx, "app-a", "session-2", "att-1"); !errors.Is(err, session.ErrAttachmentNotFound) {
		t.Fatalf("跨 Session 附件读取 error=%v, want ErrAttachmentNotFound", err)
	}
	got, err := store.GetAttachment(ctx, "app-a", "session-1", "att-1")
	if err != nil || got.Ref.Filename != "报告.pdf" || got.Ref.Size != 100 || got.MessageID != "msg-1" {
		t.Fatalf("读取附件=%#v err=%v", got, err)
	}
}

func TestBlobModeMessagePersistsOnlyReference(t *testing.T) {
	store := newSessionStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSession(t, store, "app-a", "session-1")

	message := testMessage("app-a", "session-1", "msg-1", "", now)
	message.Type = session.MessageTypeImage
	message.ContentRef = session.ContentRef{Mode: session.ContentModeBlob, BlobID: "messages/msg-1", Size: 100}
	if _, created, err := store.CreateMessage(ctx, message, nil); err != nil || !created {
		t.Fatalf("Blob 模式写入 created=%t err=%v", created, err)
	}
	if _, err := store.GetMessageContent(ctx, "app-a", "session-1", "msg-1"); !errors.Is(err, session.ErrMessageContentBlob) {
		t.Fatalf("Blob 模式正文读取 error=%v, want ErrMessageContentBlob", err)
	}
	// Blob 模式不允许携带内联正文。
	if _, _, err := store.CreateMessage(ctx, message, []byte("不应出现的内联正文")); !errors.Is(err, session.ErrInvalidMessage) {
		t.Fatalf("Blob 模式携带内联正文 error=%v, want ErrInvalidMessage", err)
	}
}
