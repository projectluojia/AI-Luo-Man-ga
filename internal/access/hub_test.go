package access

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sampleMessage() InboundMessage {
	return InboundMessage{
		AppID:             "campus-services",
		Platform:          "qq",
		PlatformChannel:   "group",
		PlatformSpaceID:   "group-100",
		PlatformUserID:    "qq-user-1",
		PlatformMessageID: "message-1",
		PlatformSessionID: "group-100",
		MessageType:       "text",
		Text:              "有哪些校巴线路？",
		OccurredAt:        time.Now().UTC(),
		IdempotencyKey:    "message-1",
	}
}

type stubResolver struct {
	users map[string]string
	err   error
}

func (s stubResolver) ResolveIdentity(_ context.Context, appID, platform, spaceID, platformUserID string) (identity.IdentityContext, error) {
	if s.err != nil {
		return identity.IdentityContext{}, s.err
	}
	userID := s.users[platform+"/"+spaceID+"/"+platformUserID]
	if userID == "" {
		return identity.IdentityContext{}, identity.ErrNotFound
	}
	return identity.IdentityContext{
		AppID: appID, UserID: userID,
		Membership: &identity.AppMembership{AppID: appID, UserID: userID},
	}, nil
}

func resolverFor(entries map[string]string) stubResolver {
	return stubResolver{users: entries}
}

func mustHub(t *testing.T, store *sqlite.Store, resolver IdentityResolver) *Hub {
	t.Helper()
	hub, err := NewHub("campus-services", store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

func TestNewHubRejectsMissingIdentityResolver(t *testing.T) {
	store := openHubStore(t)
	if _, err := NewHub("campus-services", store, nil); !errors.Is(err, ErrHubConfiguration) {
		t.Fatalf("got %v, want ErrHubConfiguration", err)
	}
}

func TestIntakeRejectsMissingIdentityBeforePersistence(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, resolverFor(nil))
	message := sampleMessage()
	message.PlatformUserID = ""
	if _, err := hub.Intake(context.Background(), message); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("got %v, want ErrIdentityRequired", err)
	}
	if _, err := store.GetSession(context.Background(), "campus-services", "web-anonymous"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("anonymous session unexpectedly exists: %v", err)
	}
}

func TestIntakeCreatesVersionedGroupSessionAndDeduplicates(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, resolverFor(map[string]string{"qq/group-100/qq-user-1": "user-1"}))
	first, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil || !first.Created {
		t.Fatalf("first intake=%#v err=%v", first, err)
	}
	if !strings.HasPrefix(first.SessionID, "session-v1-") || len(first.SessionID) != 75 || strings.Contains(first.SessionID, "group-100") {
		t.Fatalf("unsafe session id=%q", first.SessionID)
	}
	second, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil || second.Created || second.SessionID != first.SessionID {
		t.Fatalf("replayed intake=%#v err=%v", second, err)
	}
	stored, err := store.GetSession(context.Background(), "campus-services", first.SessionID)
	if err != nil || stored.Type != session.SessionTypeGroup || len(stored.Members) != 1 || stored.Members[0].Role != session.MemberRoleMember {
		t.Fatalf("stored session=%#v err=%v", stored, err)
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", first.SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "user-1" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestIntakeIsolatesGroupsPrivateAndPlatforms(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, resolverFor(map[string]string{
		"qq/group-100/qq-user-1":  "user-1",
		"qq/group-200/qq-user-1":  "user-1",
		"qq/private/qq-user-1":    "user-1",
		"cli/terminal/cli-user-1": "user-1",
	}))
	messages := []InboundMessage{sampleMessage(), sampleMessage(), sampleMessage(), sampleMessage()}
	messages[1].PlatformSpaceID = "group-200"
	messages[1].PlatformSessionID = "group-200"
	messages[2].PlatformChannel = "private"
	messages[2].PlatformSpaceID = "private"
	messages[2].PlatformSessionID = "qq-user-1"
	messages[3].Platform = "cli"
	messages[3].PlatformChannel = "private"
	messages[3].PlatformSpaceID = "terminal"
	messages[3].PlatformUserID = "cli-user-1"
	messages[3].PlatformSessionID = "cli-user-1"
	seen := make(map[string]struct{})
	for index := range messages {
		messages[index].PlatformMessageID = "message-" + string(rune('1'+index))
		messages[index].IdempotencyKey = messages[index].PlatformMessageID
		result, err := hub.Intake(context.Background(), messages[index])
		if err != nil {
			t.Fatal(err)
		}
		seen[result.SessionID] = struct{}{}
	}
	if len(seen) != 4 {
		t.Fatalf("isolated conversations produced %d sessions: %#v", len(seen), seen)
	}
}

func TestIntakeGroupSessionAddsResolvedMembersAtomically(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, resolverFor(map[string]string{
		"qq/group-100/qq-user-1": "user-1",
		"qq/group-100/qq-user-2": "user-2",
	}))
	first, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil {
		t.Fatal(err)
	}
	secondMessage := sampleMessage()
	secondMessage.PlatformUserID = "qq-user-2"
	secondMessage.PlatformMessageID = "message-2"
	secondMessage.IdempotencyKey = "message-2"
	second, err := hub.Intake(context.Background(), secondMessage)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("group members split across sessions: %q != %q", second.SessionID, first.SessionID)
	}
	stored, err := store.GetSession(context.Background(), "campus-services", first.SessionID)
	if err != nil || len(stored.Members) != 2 {
		t.Fatalf("stored session=%#v err=%v", stored, err)
	}
}

func TestIntakeDoesNotReadLegacySessionHistory(t *testing.T) {
	store := openHubStore(t)
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), session.Session{
		AppID: "campus-services", SessionID: "session-user-1", Type: session.SessionTypeDirect,
		Members:          []session.Member{{UserID: "user-1", Role: session.MemberRoleOwner, JoinedAt: now}},
		PlatformBindings: []session.PlatformBinding{{Platform: "qq", PlatformID: "legacy", BoundAt: now}},
		CreatedAt:        now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateMessage(context.Background(), session.Message{
		AppID: "campus-services", SessionID: "session-user-1", MessageID: "legacy-message",
		SenderUserID: "user-1", Type: session.MessageTypeText,
		ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len([]byte("旧历史")))}, CreatedAt: now,
	}, []byte("旧历史")); err != nil {
		t.Fatal(err)
	}
	hub := mustHub(t, store, resolverFor(map[string]string{"qq/group-100/qq-user-1": "user-1"}))
	result, err := hub.Intake(context.Background(), sampleMessage())
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID == "session-user-1" {
		t.Fatal("new message reused legacy session")
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", result.SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].MessageID == "legacy-message" {
		t.Fatalf("new session messages=%#v err=%v", messages, err)
	}
}

func TestIntakeRejectsMissingMembership(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, identityResolverFunc(func(_ context.Context, _, _, _, _ string) (identity.IdentityContext, error) {
		return identity.IdentityContext{AppID: "campus-services", UserID: "user-1"}, nil
	}))
	if _, err := hub.Intake(context.Background(), sampleMessage()); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("got %v, want ErrMembershipRequired", err)
	}
}

type identityResolverFunc func(context.Context, string, string, string, string) (identity.IdentityContext, error)

func (function identityResolverFunc) ResolveIdentity(ctx context.Context, appID, platform, spaceID, userID string) (identity.IdentityContext, error) {
	return function(ctx, appID, platform, spaceID, userID)
}

func TestIntakeRejectsAppMismatchAndInvalidMessage(t *testing.T) {
	store := openHubStore(t)
	hub := mustHub(t, store, resolverFor(map[string]string{"qq/group-100/qq-user-1": "user-1"}))
	message := sampleMessage()
	message.AppID = "other-app"
	if _, err := hub.Intake(context.Background(), message); !errors.Is(err, ErrAppMismatch) {
		t.Fatalf("got %v, want ErrAppMismatch", err)
	}
	cases := []struct {
		name   string
		mutate func(*InboundMessage)
	}{
		{name: "missing channel", mutate: func(message *InboundMessage) { message.PlatformChannel = "" }},
		{name: "missing session", mutate: func(message *InboundMessage) { message.PlatformSessionID = "" }},
		{name: "missing message id", mutate: func(message *InboundMessage) { message.PlatformMessageID = "" }},
		{name: "missing message type", mutate: func(message *InboundMessage) { message.MessageType = "" }},
		{name: "missing idempotency key", mutate: func(message *InboundMessage) { message.IdempotencyKey = "" }},
		{name: "oversized text", mutate: func(message *InboundMessage) { message.Text = strings.Repeat("字", MaxTextBytes+1) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			invalid := sampleMessage()
			test.mutate(&invalid)
			if _, err := hub.Intake(context.Background(), invalid); !errors.Is(err, session.ErrInvalidMessage) {
				t.Fatalf("got %v, want session.ErrInvalidMessage", err)
			}
		})
	}
}
