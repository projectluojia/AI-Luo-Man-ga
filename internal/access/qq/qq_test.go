package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// fakeOneBot 是 OneBot v11 WebSocket 服务端测试桩：记录收到的动作，可注入事件。
type fakeOneBot struct {
	server     *httptest.Server
	authHeader chan string
	conns      chan *websocket.Conn
	received   chan map[string]any
	upgrader   websocket.Upgrader
}

func newFakeOneBot(t *testing.T) *fakeOneBot {
	t.Helper()
	bot := &fakeOneBot{
		authHeader: make(chan string, 1),
		conns:      make(chan *websocket.Conn, 1),
		received:   make(chan map[string]any, 8),
		upgrader:   websocket.Upgrader{},
	}
	bot.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bot.authHeader <- request.Header.Get("Authorization")
		conn, err := bot.upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		bot.conns <- conn
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var action map[string]any
			if json.Unmarshal(data, &action) == nil {
				bot.received <- action
			}
		}
	}))
	t.Cleanup(bot.server.Close)
	return bot
}

func (b *fakeOneBot) wsURL() string {
	return "ws://" + b.server.Listener.Addr().String()
}

func (b *fakeOneBot) sendEvent(t *testing.T, payload string) {
	b.sendEvents(t, payload)
}

func (b *fakeOneBot) sendEvents(t *testing.T, payloads ...string) {
	t.Helper()
	select {
	case conn := <-b.conns:
		for _, payload := range payloads {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Fatalf("send event: %v", err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not establish websocket")
	}
}

type blockingEchoStarter struct {
	started chan string
	release chan struct{}
}

func (s *blockingEchoStarter) CreateIdempotent(ctx context.Context, request kernelecho.RunRequest) (string, bool, error) {
	select {
	case s.started <- request.IdempotencyKey:
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	select {
	case <-s.release:
		return "", false, errors.New("test release")
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// stubResolver 是 IdentityResolver 测试桩。
type stubResolver struct {
	user string
	err  error
}

func (r stubResolver) ResolveIdentity(context.Context, string, string, string, string) (identity.IdentityContext, error) {
	if r.err != nil {
		return identity.IdentityContext{}, r.err
	}
	return identity.IdentityContext{
		AppID: "campus-services", UserID: r.user,
		Membership: &identity.AppMembership{AppID: "campus-services", UserID: r.user},
	}, nil
}

type testProvisioner struct{}

func (testProvisioner) EnsureQQIdentity(context.Context, access.InboundMessage) error { return nil }

func newQQTestHub(t *testing.T, store *sqlite.Store, resolver access.IdentityResolver) *access.Hub {
	t.Helper()
	hub, err := access.NewHub("campus-services", store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

// qqFakeOrchestrator 是 EchoStarter 测试桩：真实写入 sqlite，成功后发信号。
type qqFakeOrchestrator struct {
	store       *sqlite.Store
	created     chan struct{}
	createdOnce sync.Once
	echoID      string
}

func (f *qqFakeOrchestrator) CreateIdempotent(ctx context.Context, request kernelecho.RunRequest) (string, bool, error) {
	id := uuid.NewString()
	now := time.Now().UTC()
	echoID, created, err := f.store.CreateEchoRunIdempotentLimited(
		ctx, request.IdempotencyKey, idempotency.Fingerprint([]byte(request.Message)),
		kernelecho.Record{ID: id, AppID: "campus-services", InputMessage: request.Message, Status: kernelecho.StatusRunning, CreatedAt: now},
		kernelecho.RunRecord{
			ID: "run-" + id, RunGroupID: "run-" + id, AppID: "campus-services", EchoID: id, Attempt: 1,
			Status: kernelecho.RunStatusQueued, Model: "test-model", ModelConfigVersion: "test-config",
			ProtocolVersion: "1.0", MaxSteps: 4, MaxToolCalls: 4,
			MaxInputTokens: 1000, MaxOutputTokens: 1000, MaxTotalTokens: 2000,
			MaxOutputBytes: 4096, ProviderTimeoutMS: 5000, Deadline: now.Add(time.Minute), AvailableAt: now,
			RecoverableState: []byte(`{}`), CreatedAt: now,
		},
		0,
	)
	if err != nil {
		return "", false, err
	}
	f.echoID = echoID
	f.createdOnce.Do(func() { close(f.created) })
	return echoID, created, nil
}

func TestNewRejectsInvalidBotQQID(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	orchestrator := &qqFakeOrchestrator{store: store, created: make(chan struct{})}
	for _, botQQID := range []string{"", " 2647414417", "02647414417", "-1", "all", "0"} {
		t.Run(botQQID, func(t *testing.T) {
			_, err := New(Config{
				AppID: "campus-services", WSURL: "ws://127.0.0.1:1", BotQQID: botQQID,
				Provisioner: testProvisioner{},
			}, hub, access.NewEventHub(), orchestrator, store)
			if err == nil {
				t.Fatal("invalid bot QQ ID was accepted")
			}
		})
	}
}

// TestQQAdapterIntakesAndReplies 验证全链路：群消息事件 → 标准入站 →
// 幂等 Echo → 订阅终态 → send_group_msg 回发。事件经 store 重放与实时
// 发布双路径送达，消除订阅时序竞态。
func TestQQAdapterIntakesAndReplies(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bot := newFakeOneBot(t)
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	events := access.NewEventHub()
	orchestrator := &qqFakeOrchestrator{store: store, created: make(chan struct{})}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), Token: "secret", BotQQID: testBotQQID,
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, events, orchestrator, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = adapter.Run(ctx) }()

	select {
	case header := <-bot.authHeader:
		if header != "Bearer secret" {
			t.Fatalf("authorization header=%q, want Bearer secret", header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not connect")
	}

	bot.sendEvent(t, `{"post_type":"message","message_type":"group","group_id":12345,"user_id":67890,"message_id":"qq-msg-1","message":[{"type":"at","data":{"qq":"2647414417"}},{"type":"text","data":{"text":"有哪些线路"}}]}`)

	select {
	case <-orchestrator.created:
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrator was not invoked")
	}
	// 事件双路径送达：先按真实调度流程认领 Run（running），再写持久化事件
	// 并发布实时事件。
	now := time.Now().UTC()
	run, err := store.ClaimRun(ctx, "campus-services", orchestrator.echoID, "lease-test", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	event := kernelecho.Event{
		AppID: "campus-services", EchoID: orchestrator.echoID, RunID: run.ID,
		Type: "reply.final", Payload: []byte(`{"text":"你好呀"}`), CreatedAt: now,
	}
	stored, err := store.AppendEchoEvent(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	events.Publish(stored)

	select {
	case action := <-bot.received:
		if action["action"] != "send_group_msg" {
			t.Fatalf("action=%#v", action)
		}
		params, _ := action["params"].(map[string]any)
		if params["group_id"] != float64(12345) {
			t.Fatalf("params=%#v", params)
		}
		assertGroupReply(t, params["message"], "67890", "你好呀")
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not send the reply")
	}
}

func assertGroupReply(t *testing.T, raw any, expectedUserID, expectedText string) {
	t.Helper()
	segments, ok := raw.([]any)
	if !ok || len(segments) != 2 {
		t.Fatalf("group reply segments=%#v", raw)
	}
	atSegment, _ := segments[0].(map[string]any)
	atData, _ := atSegment["data"].(map[string]any)
	textSegment, _ := segments[1].(map[string]any)
	textData, _ := textSegment["data"].(map[string]any)
	if atSegment["type"] != "at" || atData["qq"] != expectedUserID ||
		textSegment["type"] != "text" || !strings.Contains(textData["text"].(string), expectedText) {
		t.Fatalf("group reply segments=%#v", segments)
	}
}

// TestQQAdapterProcessesEchoesConcurrently 验证 WebSocket 读取循环不会等待单个
// Echo 终态，且同时处理的 Echo 数量受固定 worker 上限约束。
func TestQQAdapterProcessesEchoesConcurrently(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bot := newFakeOneBot(t)
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	orchestrator := &blockingEchoStarter{
		started: make(chan string, echoWorkerCount+1),
		release: make(chan struct{}),
	}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: testBotQQID,
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), orchestrator, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = adapter.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("adapter did not stop")
		}
	})

	select {
	case <-bot.authHeader:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not connect")
	}
	payloads := make([]string, 0, echoWorkerCount+1)
	for index := 0; index < echoWorkerCount+1; index++ {
		payloads = append(payloads, fmt.Sprintf(`{"post_type":"message","message_type":"private","user_id":67890,"message_id":"qq-concurrent-%d","message":[{"type":"text","data":{"text":"消息%d"}}]}`, index, index))
	}
	bot.sendEvents(t, payloads...)
	for index := 0; index < echoWorkerCount; index++ {
		select {
		case <-orchestrator.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d Echoes started concurrently", index)
		}
	}
	select {
	case <-orchestrator.started:
		t.Fatalf("Echo concurrency exceeded worker limit %d", echoWorkerCount)
	case <-time.After(200 * time.Millisecond):
	}
	close(orchestrator.release)
	select {
	case <-orchestrator.started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued Echo did not start after a worker became available")
	}
}

// TestQQAdapterRepliesPublicErrorOnUnboundIdentity 验证未绑定身份的消息被
// 安全拒绝并回发公共错误，不泄露内部细节。
func TestQQAdapterRepliesPublicErrorOnUnboundIdentity(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-unbound.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bot := newFakeOneBot(t)
	hub := newQQTestHub(t, store, stubResolver{err: identity.ErrNotFound})
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: testBotQQID,
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), &qqFakeOrchestrator{store: store, created: make(chan struct{})}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = adapter.Run(ctx) }()

	select {
	case <-bot.authHeader:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not connect")
	}
	bot.sendEvent(t, `{"post_type":"message","message_type":"private","user_id":67890,"message_id":"qq-msg-2","message":[{"type":"text","data":{"text":"你好"}}]}`)

	select {
	case action := <-bot.received:
		if action["action"] != "send_private_msg" {
			t.Fatalf("action=%#v", action)
		}
		params, _ := action["params"].(map[string]any)
		message, _ := params["message"].(string)
		if !strings.Contains(message, "平台身份未绑定") {
			t.Fatalf("message=%q", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not send the error reply")
	}
}
