package contextasm_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contextasm"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

const testAppID = "campus-services"

func newHarness(t *testing.T) (*sqlite.Store, *session.Service) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
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
	return store, service
}

// ensureSession 幂等创建测试会话：已存在则复用（时间戳不同不视为冲突）。
func ensureSession(t *testing.T, store *sqlite.Store, appID, sessionID string) {
	t.Helper()
	if _, err := store.GetSession(context.Background(), appID, sessionID); err == nil {
		return
	}
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), session.Session{
		AppID: appID, SessionID: sessionID, Type: session.SessionTypeDirect,
		Members:   []session.Member{{UserID: "anonymous", Role: session.MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// createMessage 写入一条标准消息（先确保会话存在）并检查错误。
func createMessage(t *testing.T, store *sqlite.Store, service *session.Service, message session.Message, content []byte) {
	t.Helper()
	ensureSession(t, store, message.AppID, message.SessionID)
	if _, _, err := service.CreateMessage(context.Background(), message, content); err != nil {
		t.Fatal(err)
	}
}

// seedSession 创建会话并按文本顺序写入历史消息，消息标识为 message-0、message-1、
// ……，时间从 0 分钟起逐条递增（message-0 最旧，最后一条最新）。
func seedSession(t *testing.T, store *sqlite.Store, service *session.Service, sessionID string, texts ...string) {
	t.Helper()
	now := time.Now().UTC()
	ensureSession(t, store, testAppID, sessionID)
	for index, text := range texts {
		createMessage(t, store, service, session.Message{
			AppID: testAppID, SessionID: sessionID, MessageID: "message-" + strconv.Itoa(index),
			SenderUserID: "anonymous", Type: session.MessageTypeText,
			ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len([]byte(text)))},
			CreatedAt:  now.Add(time.Duration(index) * time.Minute),
		}, []byte(text))
	}
}

func assemble(t *testing.T, assembler *contextasm.Assembler, in contextasm.Input) contextasm.Snapshot {
	t.Helper()
	snapshot, err := assembler.Assemble(context.Background(), in)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return snapshot
}

func input(sessionID, currentMessageID, message string) contextasm.Input {
	return contextasm.Input{
		AppID:            testAppID,
		SessionID:        sessionID,
		CurrentMessageID: currentMessageID,
		ConfigRevision:   "config-revision-1",
		SystemPrompt:     "你是校园综合服务智能体。",
		Timezone:         "Asia/Shanghai",
		Capabilities:     []string{"campus.bus.routes.list@1.0.0", "campus.bus.journeys.search@1.0.0"},
		InputMessage:     message,
		Now:              time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
	}
}

func TestAssembleDeterministicDigest(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "早上好", "校巴几点发车")
	assembler, err := contextasm.New(service, contextasm.DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	first := assemble(t, assembler, input("session-1", "message-1", "请查询线路"))
	second := assemble(t, assembler, input("session-1", "message-1", "请查询线路"))
	if first.Digest != second.Digest {
		t.Fatalf("相同配置修订与数据版本摘要不一致: %s != %s", first.Digest, second.Digest)
	}
	if first.SystemPrompt != second.SystemPrompt {
		t.Fatalf("相同输入渲染不一致: %q != %q", first.SystemPrompt, second.SystemPrompt)
	}
	changed := assemble(t, assembler, input("session-1", "message-1", "查询到站时间"))
	if changed.Digest == first.Digest {
		t.Fatal("当前消息变化后摘要必须变化")
	}
}

func TestAssembleExcludesCurrentMessage(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "之前的问题", "当前的问题")
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "message-1", "当前的问题"))
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].MessageID != "message-0" {
		t.Fatalf("当前消息必须从历史排除: %#v", snapshot.History.Entries)
	}
	if strings.Contains(snapshot.SystemPrompt, "当前的问题") {
		t.Fatalf("当前消息不得重复进入历史块: %q", snapshot.SystemPrompt)
	}
	if !strings.Contains(snapshot.SystemPrompt, "之前的问题") {
		t.Fatalf("历史正文必须进入系统提示: %q", snapshot.SystemPrompt)
	}
}

func TestAssembleAppIsolation(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "本应用历史")
	createMessage(t, store, service, session.Message{
		AppID: "other-app", SessionID: "other-session", MessageID: "other-1",
		SenderUserID: "anonymous", Type: session.MessageTypeText,
		ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: 12},
		CreatedAt:  time.Now().UTC(),
	}, []byte("别的历史"))
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].MessageID != "message-0" {
		t.Fatalf("跨 App 消息混入历史: %#v", snapshot.History.Entries)
	}
	if strings.Contains(snapshot.SystemPrompt, "别的历史") {
		t.Fatalf("跨 App 正文进入提示: %q", snapshot.SystemPrompt)
	}
}

func TestAssembleDeletedMessageExcluded(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "保留的消息", "待撤回的消息")
	if err := store.RecallMessage(context.Background(), testAppID, "session-1", "message-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].MessageID != "message-0" {
		t.Fatalf("已删除消息不得投影: %#v", snapshot.History.Entries)
	}
}

func TestAssembleEntryCountBudgetDropsOldest(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "一", "二", "三", "四")
	assembler, _ := contextasm.New(service, contextasm.Budget{MaxMessages: 2})
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 2 {
		t.Fatalf("条目预算后历史=%d，期望 2", len(snapshot.History.Entries))
	}
	if snapshot.History.Entries[0].MessageID != "message-2" || snapshot.History.Entries[1].MessageID != "message-3" {
		t.Fatalf("保留最新两条: %#v", snapshot.History.Entries)
	}
	if snapshot.History.Trimmed != 1 {
		t.Fatalf("裁剪数=%d，期望 1（最旧候选 message-1 被条目预算丢弃）", snapshot.History.Trimmed)
	}
}

func TestAssembleTotalCharsBudgetDropsOldest(t *testing.T) {
	store, service := newHarness(t)
	long := strings.Repeat("长", 100)
	seedSession(t, store, service, "session-1", long, "短")
	assembler, _ := contextasm.New(service, contextasm.Budget{MaxTotalChars: 50})
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].MessageID != "message-1" {
		t.Fatalf("字符预算应丢弃最旧长消息: %#v", snapshot.History.Entries)
	}
	if snapshot.History.Trimmed != 1 {
		t.Fatalf("裁剪数=%d，期望 1", snapshot.History.Trimmed)
	}
}

func TestAssemblePerMessageCharBudgetTruncates(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", strings.Repeat("很", 100))
	assembler, _ := contextasm.New(service, contextasm.Budget{MaxCharsPerMsg: 10})
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 1 {
		t.Fatal("截断不改变条目数")
	}
	if got := len([]rune(snapshot.History.Entries[0].Content)); got != 10 {
		t.Fatalf("单条截断后字符=%d，期望 10", got)
	}
	if snapshot.History.TruncatedChars != 90 {
		t.Fatalf("截断字符=%d，期望 90", snapshot.History.TruncatedChars)
	}
}

func TestAssembleMaxAgeBudget(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "很旧的消息", "很新的消息")
	assembler, _ := contextasm.New(service, contextasm.Budget{MaxAge: 90 * time.Second})
	in := input("session-1", "", "请查询")
	in.Now = time.Now().UTC().Add(2 * time.Minute) // 消息位于 now-2min 与 now-1min
	snapshot := assemble(t, assembler, in)
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].MessageID != "message-1" {
		t.Fatalf("时间预算应排除超龄消息: %#v", snapshot.History.Entries)
	}
}

func TestAssembleNoSessionSkipsHistory(t *testing.T) {
	_, service := newHarness(t)
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("", "", "子任务"))
	if len(snapshot.History.Entries) != 0 {
		t.Fatalf("无会话历史必须为空: %#v", snapshot.History.Entries)
	}
	replay := assemble(t, assembler, input("", "", "子任务"))
	if replay.Digest != snapshot.Digest {
		t.Fatalf("无会话快照摘要不确定: %s != %s", replay.Digest, snapshot.Digest)
	}
}

func TestAssembleBlobMessageContent(t *testing.T) {
	store, service := newHarness(t)
	now := time.Now().UTC()
	createMessage(t, store, service, session.Message{
		AppID: testAppID, SessionID: "session-1", MessageID: "blob-1",
		SenderUserID: "anonymous", Type: session.MessageTypeText,
		ContentRef: session.ContentRef{Mode: session.ContentModeBlob, BlobID: "messages/blob-1", Size: 21},
		CreatedAt:  now,
	}, []byte("二进制正文内容"))
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 1 || snapshot.History.Entries[0].Content != "二进制正文内容" {
		t.Fatalf("Blob 模式正文未装配: %#v", snapshot.History.Entries)
	}
	if snapshot.History.Entries[0].ContentSHA == "" {
		t.Fatal("Blob 消息正文摘要缺失")
	}
}

func TestAssembleNonTextMessagesUsePlaceholder(t *testing.T) {
	store, service := newHarness(t)
	now := time.Now().UTC()
	messages := []session.Message{
		{AppID: testAppID, SessionID: "session-1", MessageID: "image-1", SenderUserID: "anonymous",
			Type: session.MessageTypeImage, ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: 4}, CreatedAt: now},
		{AppID: testAppID, SessionID: "session-1", MessageID: "system-1", SenderUserID: "anonymous",
			Type: session.MessageTypeSystem, ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: 4}, CreatedAt: now.Add(time.Minute)},
	}
	for _, message := range messages {
		createMessage(t, store, service, message, []byte("abcd"))
	}
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 2 {
		t.Fatalf("非文本消息应占位进入历史: %#v", snapshot.History.Entries)
	}
	if snapshot.History.Entries[0].Sender != "图片消息" || snapshot.History.Entries[0].Content != "" {
		t.Fatalf("图片消息占位错误: %#v", snapshot.History.Entries[0])
	}
	if snapshot.History.Entries[1].Sender != "系统消息" {
		t.Fatalf("系统消息占位错误: %#v", snapshot.History.Entries[1])
	}
}

func TestAssemblePromptBudgetDropsHistoryThenFails(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "历史一", "历史二")
	// 提示总预算仅容纳基础提示：历史必须被全部丢弃，不静默超过预算。
	assembler, _ := contextasm.New(service, contextasm.Budget{MaxPromptBytes: 128})
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if len(snapshot.History.Entries) != 0 || snapshot.History.Trimmed != 2 {
		t.Fatalf("提示预算应丢弃全部历史: %#v", snapshot.History)
	}
	// 基础提示本身超出预算：显式失败，不静默裁剪配置。
	oversized, err := contextasm.New(service, contextasm.Budget{MaxPromptBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oversized.Assemble(context.Background(), input("session-1", "", "请查询")); err != contextasm.ErrPromptBudgetExceeded {
		t.Fatalf("基础提示超预算错误=%v，期望 ErrPromptBudgetExceeded", err)
	}
}

func TestAssembleHistoryReadFailure(t *testing.T) {
	assembler, err := contextasm.New(failingSource{}, contextasm.DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.Assemble(context.Background(), input("session-1", "", "请查询")); err == nil {
		t.Fatal("历史读取失败必须返回错误")
	}
}

func TestNewRejectsNilSourceAndInvalidBudget(t *testing.T) {
	if _, err := contextasm.New(nil, contextasm.DefaultBudget()); err == nil {
		t.Fatal("nil 历史来源必须被拒绝")
	}
	oversizedPrompt := contextasm.Budget{MaxPromptBytes: 33 << 10}
	if _, err := contextasm.New(validSource{}, oversizedPrompt); err == nil {
		t.Fatal("超过协议上限的提示预算必须被拒绝")
	}
}

func TestAssembleSourcesContract(t *testing.T) {
	store, service := newHarness(t)
	seedSession(t, store, service, "session-1", "历史消息")
	assembler, _ := contextasm.New(service, contextasm.DefaultBudget())
	snapshot := assemble(t, assembler, input("session-1", "", "请查询"))
	if snapshot.Sources.Config.Version != "config-revision-1" {
		t.Fatalf("配置来源版本错误: %#v", snapshot.Sources.Config)
	}
	if snapshot.Sources.History.Count != 1 || snapshot.Sources.History.Version == "" {
		t.Fatalf("历史来源版本错误: %#v", snapshot.Sources.History)
	}
	if snapshot.Sources.Capabilities.Count != 2 || snapshot.Sources.Capabilities.Version == "" {
		t.Fatalf("Capability 来源版本错误: %#v", snapshot.Sources.Capabilities)
	}
	encoded := string(snapshot.SourcesJSON())
	if strings.Contains(encoded, "历史消息") {
		t.Fatalf("来源版本不得包含正文: %s", encoded)
	}
}

type failingSource struct{}

func (failingSource) ListMessages(context.Context, string, string, session.MessageQuery) ([]session.Message, error) {
	return nil, context.DeadlineExceeded
}

func (failingSource) MessageContent(context.Context, session.Message) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

type validSource struct{}

func (validSource) ListMessages(context.Context, string, string, session.MessageQuery) ([]session.Message, error) {
	return nil, nil
}

func (validSource) MessageContent(context.Context, session.Message) ([]byte, error) {
	return nil, nil
}
