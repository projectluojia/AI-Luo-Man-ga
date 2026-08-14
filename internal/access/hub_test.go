package access

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func openHubStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sampleMessage() InboundMessage {
	return InboundMessage{
		AppID:             "campus-services",
		Platform:          "web",
		PlatformMessageID: "message-1",
		PlatformSessionID: "web-anonymous",
		MessageType:       "text",
		Text:              "有哪些校巴线路？",
		OccurredAt:        time.Now().UTC(),
		IdempotencyKey:    "message-1",
	}
}

func TestIntakePersistsAnonymousSessionAndMessage(t *testing.T) {
	store := openHubStore(t)
	hub := NewHub("campus-services", store, nil)
	result, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID != AnonymousSenderID || result.SessionID != AnonymousSessionID || result.MessageID != "message-1" || !result.Created {
		t.Fatalf("intake result=%#v", result)
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", AnonymousSessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != AnonymousSenderID {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestIntakeDeduplicatesRepeatedDelivery(t *testing.T) {
	store := openHubStore(t)
	hub := NewHub("campus-services", store, nil)
	first, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil || !first.Created {
		t.Fatalf("first intake=%#v err=%v", first, err)
	}
	second, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil || second.Created {
		t.Fatalf("重复投递应去重返回既有消息: %#v err=%v", second, err)
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", AnonymousSessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 {
		t.Fatalf("重复投递产生多条消息: %#v err=%v", messages, err)
	}
}

func TestIntakeRejectsIdentityWhenResolverNil(t *testing.T) {
	store := openHubStore(t)
	hub := NewHub("campus-services", store, nil)
	message := sampleMessage()
	message.PlatformUserID = "qq-10001"
	if _, err := hub.Intake(context.Background(), message); !errors.Is(err, ErrAnonymousOnly) {
		t.Fatalf("got %v, want ErrAnonymousOnly", err)
	}
}

type stubResolver struct {
	context identity.IdentityContext
	err     error
}

func (s stubResolver) ResolveIdentity(_ context.Context, _, _, _, _ string) (identity.IdentityContext, error) {
	return s.context, s.err
}

func TestIntakeResolvesPlatformIdentityToUserSession(t *testing.T) {
	store := openHubStore(t)
	hub := NewHub("campus-services", store, stubResolver{context: identity.IdentityContext{
		AppID: "campus-services", UserID: "user-1",
	}})
	message := sampleMessage()
	message.Platform = "qq"
	message.PlatformUserID = "qq-10001"
	result, err := hub.Intake(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID != "user-1" || result.SessionID != "session-user-1" {
		t.Fatalf("identity intake result=%#v", result)
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", "session-user-1", session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "user-1" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestIntakeRejectsAppMismatchAndInvalidMessage(t *testing.T) {
	store := openHubStore(t)
	hub := NewHub("campus-services", store, nil)
	message := sampleMessage()
	message.AppID = "other-app"
	if _, err := hub.Intake(context.Background(), message); !errors.Is(err, ErrAppMismatch) {
		t.Fatalf("got %v, want ErrAppMismatch", err)
	}
	// 校验失败必须归类为 ErrInvalidMessage，供公共错误映射输出 400（而非默认 500）。
	cases := []struct {
		name   string
		mutate func(*InboundMessage)
	}{
		{name: "missing message id", mutate: func(m *InboundMessage) { m.PlatformMessageID = "" }},
		{name: "missing message type", mutate: func(m *InboundMessage) { m.MessageType = "" }},
		{name: "missing idempotency key", mutate: func(m *InboundMessage) { m.IdempotencyKey = "" }},
		{name: "oversized text", mutate: func(m *InboundMessage) { m.Text = string(make([]byte, MaxTextBytes+1)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invalid := sampleMessage()
			tc.mutate(&invalid)
			if _, err := hub.Intake(context.Background(), invalid); !errors.Is(err, session.ErrInvalidMessage) {
				t.Fatalf("got %v, want session.ErrInvalidMessage", err)
			}
		})
	}
}
