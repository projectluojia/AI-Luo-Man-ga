package qq

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func newPokeAdapter(t *testing.T, bot *fakeOneBot) *Adapter {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-poke.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: "2647414417",
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), &qqFakeOrchestrator{store: store, created: make(chan struct{})}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = adapter.Run(ctx) }()
	select {
	case <-bot.authHeader:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not connect")
	}
	return adapter
}

// TestQQAdapterRepliesToGroupPoke 验证群聊戳机器人：回随机文案 + group_poke 戳回去。
func TestQQAdapterRepliesToGroupPoke(t *testing.T) {
	bot := newFakeOneBot(t)
	newPokeAdapter(t, bot)

	bot.sendEvent(t, `{"post_type":"notice","notice_type":"notify","sub_type":"poke","group_id":12345,"user_id":67890,"target_id":2647414417}`)

	var textSeen, pokeSeen bool
	deadline := time.After(5 * time.Second)
	for !textSeen || !pokeSeen {
		select {
		case action := <-bot.received:
			switch action["action"] {
			case "send_group_msg":
				params, _ := action["params"].(map[string]any)
				if params["group_id"] != float64(12345) {
					t.Fatalf("group reply params=%#v", params)
				}
				message, _ := params["message"].(string)
				if message == "" {
					t.Fatalf("empty or mentioned poke reply: %#v", params["message"])
				}
				textSeen = true
			case "group_poke":
				params, _ := action["params"].(map[string]any)
				if params["group_id"] != float64(12345) || params["user_id"] != float64(67890) {
					t.Fatalf("group_poke params=%#v", params)
				}
				pokeSeen = true
			default:
				t.Fatalf("unexpected action=%#v", action)
			}
		case <-deadline:
			t.Fatalf("timeout: textSeen=%t pokeSeen=%t", textSeen, pokeSeen)
		}
	}
}

// TestQQAdapterRepliesToPrivatePoke 验证私聊戳机器人：回随机文案，无 group_poke。
func TestQQAdapterRepliesToPrivatePoke(t *testing.T) {
	bot := newFakeOneBot(t)
	newPokeAdapter(t, bot)

	bot.sendEvent(t, `{"post_type":"notice","notice_type":"notify","sub_type":"poke","user_id":67890,"target_id":2647414417}`)

	select {
	case action := <-bot.received:
		if action["action"] != "send_private_msg" {
			t.Fatalf("action=%#v", action)
		}
		params, _ := action["params"].(map[string]any)
		if params["user_id"] != float64(67890) {
			t.Fatalf("params=%#v", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not reply to private poke")
	}
}

func TestQQAdapterCanDisablePokeTextWithoutDisablingGroupPoke(t *testing.T) {
	bot := newFakeOneBot(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-poke-disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: "2647414417",
		AllowedGroupIDs: []string{"12345"}, PokeReplies: []string{}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), &qqFakeOrchestrator{store: store, created: make(chan struct{})}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = adapter.Run(ctx) }()
	select {
	case <-bot.authHeader:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not connect")
	}

	bot.sendEvent(t, `{"post_type":"notice","notice_type":"notify","sub_type":"poke","group_id":12345,"user_id":67890,"target_id":2647414417}`)

	select {
	case action := <-bot.received:
		if action["action"] != "group_poke" {
			t.Fatalf("unexpected action=%#v", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not poke back")
	}
	select {
	case action := <-bot.received:
		t.Fatalf("unexpected text reply=%#v", action)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestQQAdapterIgnoresPokeOnOthers 验证戳别人不触发回复。
func TestQQAdapterIgnoresPokeOnOthers(t *testing.T) {
	bot := newFakeOneBot(t)
	newPokeAdapter(t, bot)

	bot.sendEvent(t, `{"post_type":"notice","notice_type":"notify","sub_type":"poke","group_id":12345,"user_id":67890,"target_id":111111}`)

	select {
	case action := <-bot.received:
		t.Fatalf("unexpected action=%#v", action)
	case <-time.After(500 * time.Millisecond):
		// 预期静默：戳的不是机器人。
	}
}

// TestQQAdapterIgnoresGroupMessageWithoutMention 验证群聊未 @ 机器人时静默忽略。
func TestQQAdapterIgnoresGroupMessageWithoutMention(t *testing.T) {
	bot := newFakeOneBot(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-ignore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	orchestrator := &qqFakeOrchestrator{store: store, created: make(chan struct{})}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: "2647414417",
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), orchestrator, store)
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

	bot.sendEvent(t, `{"post_type":"message","message_type":"group","group_id":12345,"user_id":67890,"message_id":"qq-msg-ignore","message":[{"type":"text","data":{"text":"有哪些线路"}}]}`)

	select {
	case action := <-bot.received:
		t.Fatalf("unexpected action=%#v", action)
	case <-time.After(500 * time.Millisecond):
		// 预期静默：群聊未 @ 机器人。
	}
}

// TestQQAdapterIgnoresMentionOfAnotherUser 验证群聊只 @ 其他用户时不会创建 Echo。
func TestQQAdapterIgnoresMentionOfAnotherUser(t *testing.T) {
	bot := newFakeOneBot(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-other-mention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	orchestrator := &qqFakeOrchestrator{store: store, created: make(chan struct{})}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: testBotQQID,
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), orchestrator, store)
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

	bot.sendEvent(t, `{"post_type":"message","message_type":"group","group_id":12345,"user_id":67890,"message_id":"qq-msg-other-mention","message":[{"type":"at","data":{"qq":"111111"}},{"type":"text","data":{"text":"有哪些线路"}}]}`)

	select {
	case <-orchestrator.created:
		t.Fatal("mentioning another user unexpectedly created an Echo")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestQQAdapterHandlesGroupMessageWithMention 验证群聊 @ 机器人后正常入站。
func TestQQAdapterHandlesGroupMessageWithMention(t *testing.T) {
	bot := newFakeOneBot(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-mention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hub := newQQTestHub(t, store, stubResolver{user: "user-1"})
	orchestrator := &qqFakeOrchestrator{store: store, created: make(chan struct{})}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: bot.wsURL(), BotQQID: "2647414417",
		AllowedGroupIDs: []string{"12345"}, AllowedPrivateUserIDs: []string{"67890"}, Provisioner: testProvisioner{},
		DialTimeout: 2 * time.Second, ReconnectDelay: 50 * time.Millisecond, RunTimeout: 5 * time.Second,
	}, hub, access.NewEventHub(), orchestrator, store)
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

	bot.sendEvent(t, `{"post_type":"message","message_type":"group","group_id":12345,"user_id":67890,"message_id":"qq-msg-mention","message":[{"type":"at","data":{"qq":"2647414417"}},{"type":"text","data":{"text":"有哪些线路"}}]}`)

	select {
	case <-orchestrator.created:
	case <-time.After(5 * time.Second):
		t.Fatal("mentioned group message did not reach orchestrator")
	}
	if !strings.Contains(orchestrator.echoID, "-") {
		t.Fatalf("unexpected echo id=%q", orchestrator.echoID)
	}
}
