package ingress_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/ingress"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

const testAppID = "campus-services"

type testScheduler struct{}

func (testScheduler) Enqueue(context.Context, string) {}

// fakeEchoCreator 按幂等键模拟真实 orchestrator 的幂等创建。
type fakeEchoCreator struct {
	calls         int
	last          kernelecho.RunRequest
	byKey         map[string]string // 幂等键 -> echo 标识
	createErr     error
	createEntered chan struct{}
	releaseCreate chan struct{}
}

func (f *fakeEchoCreator) CreateIdempotent(_ context.Context, request kernelecho.RunRequest) (string, bool, error) {
	if f.createEntered != nil {
		close(f.createEntered)
		<-f.releaseCreate
	}
	f.calls++
	f.last = request
	if f.createErr != nil {
		return "", false, f.createErr
	}
	if f.byKey == nil {
		f.byKey = make(map[string]string)
	}
	if existing, ok := f.byKey[request.IdempotencyKey]; ok {
		return existing, false, nil
	}
	echoID := fmt.Sprintf("echo-%d", f.calls)
	f.byKey[request.IdempotencyKey] = echoID
	return echoID, true, nil
}

// harness 装配完整链路：SQLite 存储 + 身份服务 + Hub + ingress。
type harness struct {
	store  *sqlite.Store
	ids    *identity.Service
	echoes *fakeEchoCreator
	server *ingress.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "ingress.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ids := identity.NewService(store)
	hub, err := access.NewHub(testAppID, store, ids)
	if err != nil {
		t.Fatal(err)
	}
	echoes := &fakeEchoCreator{}
	return &harness{store: store, ids: ids, echoes: echoes, server: ingress.NewServer(testAppID, hub, echoes, testScheduler{})}
}

// openIdentity 开通一个平台用户：内部用户 + 平台绑定 + App 成员关系。
func (h *harness) openIdentity(t *testing.T, userID, platform, spaceID, platformUserID string) {
	t.Helper()
	if _, err := h.ids.CreateUser(context.Background(), userID); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := h.ids.BindExternalIdentity(context.Background(), identity.ExternalIdentity{
		AppID: testAppID, Platform: platform, PlatformSpaceID: spaceID,
		PlatformUserID: platformUserID, UserID: userID, BoundAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}
	if err := h.ids.SetMembership(context.Background(), identity.AppMembership{
		AppID: testAppID, UserID: userID, RoleIDs: []string{},
	}); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
}

func (h *harness) post(t *testing.T, platform, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingress/"+platform, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func sampleEvent(overrides map[string]string) string {
	event := map[string]any{
		"platform_channel":    "group",
		"platform_user_id":    "qq-user-1",
		"platform_space_id":   "qq-group-1",
		"platform_session_id": "qq-session-1",
		"platform_message_id": "qq-msg-1",
		"message_type":        "text",
		"text":                "有哪些校巴线路？",
		"occurred_at":         "2026-08-13T08:00:00Z",
		"idempotency_key":     "qq-msg-1",
	}
	for key, value := range overrides {
		if value == "" {
			delete(event, key)
		} else {
			event[key] = value
		}
	}
	payload, _ := json.Marshal(event)
	return string(payload)
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func TestIngressResolvesPlatformIdentityToUserSessionAndEcho(t *testing.T) {
	h := newHarness(t)
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")

	recorder := h.post(t, "qq", sampleEvent(nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	response := decodeResponse(t, recorder)
	sessionID, _ := response["session_id"].(string)
	if response["sender_user_id"] != "user-qq-1" || !strings.HasPrefix(sessionID, "session-v1-") ||
		response["created"] != true || response["echo_id"] != "echo-1" {
		t.Fatalf("response = %#v", response)
	}
	if h.echoes.last.Message != "有哪些校巴线路？" || h.echoes.last.IdempotencyKey != "qq-msg-1" {
		t.Fatalf("echo request = %#v", h.echoes.last)
	}
	// 消息归属身份用户的会话，不落入匿名会话。
	messages, err := h.store.ListMessages(context.Background(), testAppID, sessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "user-qq-1" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestIngressDeduplicatesRepeatedPlatformDelivery(t *testing.T) {
	h := newHarness(t)
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")

	first := h.post(t, "qq", sampleEvent(nil))
	if first.Code != http.StatusOK || decodeResponse(t, first)["created"] != true {
		t.Fatalf("first delivery status=%d body=%s", first.Code, first.Body.String())
	}
	second := h.post(t, "qq", sampleEvent(nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second delivery status=%d body=%s", second.Code, second.Body.String())
	}
	response := decodeResponse(t, second)
	if response["created"] != false || response["echo_id"] != "echo-1" {
		t.Fatalf("replayed response = %#v, want created=false with same echo", response)
	}
	// 同一平台消息只落一条标准消息。
	sessionID, _ := response["session_id"].(string)
	messages, err := h.store.ListMessages(context.Background(), testAppID, sessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%#v err=%v, want single message", messages, err)
	}
}

func TestIngressKeepsSessionAcrossDistinctMessages(t *testing.T) {
	h := newHarness(t)
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")

	first := h.post(t, "qq", sampleEvent(nil))
	firstResponse := decodeResponse(t, first)
	if first.Code != http.StatusOK || firstResponse["created"] != true {
		t.Fatalf("first delivery status=%d body=%s", first.Code, first.Body.String())
	}
	second := h.post(t, "qq", sampleEvent(map[string]string{
		"platform_message_id": "qq-msg-2",
		"idempotency_key":     "qq-msg-2",
		"text":                "末班车几点？",
	}))
	if second.Code != http.StatusOK {
		t.Fatalf("second delivery status=%d body=%s", second.Code, second.Body.String())
	}
	response := decodeResponse(t, second)
	// 同一身份用户的不同消息必须归一到同一会话，且 Echo 是新建的。
	if response["session_id"] != firstResponse["session_id"] || response["created"] != true || response["echo_id"] != "echo-2" {
		t.Fatalf("second response = %#v", response)
	}
	sessionID, _ := response["session_id"].(string)
	messages, err := h.store.ListMessages(context.Background(), testAppID, sessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%#v err=%v, want two messages in one session", messages, err)
	}
}

func TestIngressRejectsIdempotencyKeyConflict(t *testing.T) {
	h := newHarness(t)
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")

	first := h.post(t, "qq", sampleEvent(nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery status=%d body=%s", first.Code, first.Body.String())
	}
	// 同一平台消息但换用不同幂等键：平台去重键冲突 → 409，禁止改写既有消息。
	conflict := h.post(t, "qq", sampleEvent(map[string]string{"idempotency_key": "different-key"}))
	if conflict.Code != http.StatusConflict || decodeResponse(t, conflict)["code"] != "idempotency_conflict" {
		t.Fatalf("conflict status=%d body=%s, want 409 idempotency_conflict", conflict.Code, conflict.Body.String())
	}
	// 冲突请求不产生新消息、不创建新 Echo。
	if h.echoes.calls != 1 {
		t.Fatalf("echo calls = %d, want 1", h.echoes.calls)
	}
}

func TestIngressRejectsInvalidPlatformIdentity(t *testing.T) {
	h := newHarness(t)
	// 平台用户标识含控制字符：客户端错误 → 400，而不是 500。
	recorder := h.post(t, "qq", sampleEvent(map[string]string{"platform_user_id": "openid\x00evil"}))
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["code"] != "invalid_platform_identity" {
		t.Fatalf("invalid identity status=%d body=%s, want 400 invalid_platform_identity", recorder.Code, recorder.Body.String())
	}
	if h.echoes.calls != 0 {
		t.Fatalf("echo calls = %d, want 0", h.echoes.calls)
	}
}

func TestIngressRejectsUnboundAndDisabledIdentity(t *testing.T) {
	h := newHarness(t)
	// 未绑定：身份未开通 → 401 identity_not_found。
	unbound := h.post(t, "qq", sampleEvent(nil))
	if unbound.Code != http.StatusUnauthorized {
		t.Fatalf("unbound status=%d body=%s, want 401", unbound.Code, unbound.Body.String())
	}
	if decodeResponse(t, unbound)["code"] != "identity_not_found" {
		t.Fatalf("unbound body=%s", unbound.Body.String())
	}

	// 开通后禁用：403 user_disabled。
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")
	if _, err := h.ids.DisableUser(context.Background(), "user-qq-1"); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	disabled := h.post(t, "qq", sampleEvent(nil))
	if disabled.Code != http.StatusForbidden || decodeResponse(t, disabled)["code"] != "user_disabled" {
		t.Fatalf("disabled status=%d body=%s, want 403 user_disabled", disabled.Code, disabled.Body.String())
	}
}

func TestIngressRejectsMissingPlatformIdentity(t *testing.T) {
	h := newHarness(t)
	recorder := h.post(t, "qq", sampleEvent(map[string]string{"platform_user_id": ""}))
	if recorder.Code != http.StatusUnauthorized || decodeResponse(t, recorder)["code"] != "authentication_required" {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.echoes.calls != 0 {
		t.Fatalf("echo calls = %d, want 0", h.echoes.calls)
	}
}

func TestIngressRejectsMalformedEvents(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		body string
	}{
		{name: "invalid platform", body: sampleEvent(nil)},
		{name: "bad json", body: `{"platform_message_id":`},
		{name: "unknown field", body: `{"platform_message_id":"m","message_type":"text","idempotency_key":"k","text":"x","hacked":true}`},
		{name: "missing message id", body: sampleEvent(map[string]string{"platform_message_id": ""})},
		{name: "missing message type", body: sampleEvent(map[string]string{"message_type": ""})},
		{name: "missing idempotency key", body: sampleEvent(map[string]string{"idempotency_key": ""})},
		{name: "oversized text", body: sampleEvent(map[string]string{"text": strings.Repeat("字", 4001)})},
		{name: "extra json", body: `{"platform_message_id":"m","message_type":"text","idempotency_key":"k","text":"x"}{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			platform := "qq"
			if tc.name == "invalid platform" {
				platform = "QQ-UPPER"
			}
			recorder := h.post(t, platform, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", recorder.Code, recorder.Body.String())
			}
		})
	}
	// 非法平台标识不改写标准消息。
	if h.echoes.calls != 0 {
		t.Fatalf("echo calls = %d, want 0", h.echoes.calls)
	}
}

func TestIngressMapsEchoCreationErrors(t *testing.T) {
	h := newHarness(t)
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")
	h.echoes.createErr = kernelecho.ErrQueueFull
	recorder := h.post(t, "qq", sampleEvent(nil))
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("queue full status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if h.echoes.createErr = errors.New("boom"); h.post(t, "qq", sampleEvent(nil)).Code != http.StatusInternalServerError {
		t.Fatal("internal echo error should map to 500")
	}
}

func TestIngressShutdownWaitsForAdmittedCreation(t *testing.T) {
	h := newHarness(t)
	h.echoes.createEntered = make(chan struct{})
	h.echoes.releaseCreate = make(chan struct{})
	h.openIdentity(t, "user-qq-1", "qq", "qq-group-1", "qq-user-1")

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingress/qq", strings.NewReader(sampleEvent(nil)))
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(response, request)
		responseDone <- response
	}()
	<-h.echoes.createEntered
	h.server.StopAccepting()
	shutdownDone := make(chan error, 1)
	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { shutdownDone <- h.server.WaitAdmissions(shutdownContext) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown did not wait for admitted ingress: %v", err)
	default:
	}
	close(h.echoes.releaseCreate)
	response := <-responseDone
	if response.Code != http.StatusOK {
		t.Fatalf("admitted ingress status=%d body=%s", response.Code, response.Body.String())
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	late := h.post(t, "qq", sampleEvent(map[string]string{"platform_message_id": "late", "idempotency_key": "late"}))
	if late.Code != http.StatusServiceUnavailable || decodeResponse(t, late)["code"] != "shutting_down" {
		t.Fatalf("late ingress status=%d body=%s", late.Code, late.Body.String())
	}
}
