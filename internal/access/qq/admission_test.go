package qq

import (
	"context"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
)

type countingProvisioner struct{ calls int }

func (p *countingProvisioner) EnsureQQIdentity(context.Context, access.InboundMessage) error {
	p.calls++
	return nil
}

func TestQQAdmissionRejectsUnknownGroupBeforeHub(t *testing.T) {
	store := newQQTestStore(t, "qq-admission.db")
	hub, err := access.NewHub("campus-services", store, stubResolver{user: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &countingProvisioner{}
	adapter, err := New(Config{
		AppID: "campus-services", WSURL: "ws://127.0.0.1:3001", BotQQID: testBotQQID,
		AllowedGroupIDs: []string{"12345"}, Provisioner: provisioner, Admission: newQQAdmission(&qqFakeOrchestrator{store: store, created: make(chan struct{})}),
	}, hub, access.NewEventHub(), store)
	if err != nil {
		t.Fatal(err)
	}
	adapter.handleEvent(context.Background(), map[string]any{
		"post_type": "message", "message_type": "group", "group_id": "99999", "user_id": "67890", "message_id": "not-allowed",
		"message": []any{map[string]any{"type": "at", "data": map[string]any{"qq": testBotQQID}}, map[string]any{"type": "text", "data": map[string]any{"text": "你好"}}},
	})
	if provisioner.calls != 0 {
		t.Fatalf("未允许群消息触发了身份开通：%d", provisioner.calls)
	}
	if _, _, err := store.GetEcho(context.Background(), "campus-services", "not-allowed"); err == nil {
		t.Fatal("未允许群消息创建了 Echo")
	}
}
