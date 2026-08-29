package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
)

func newConfirmationTestServer(t *testing.T) (http.Handler, *sqlite.Store, *confirmation.Service) {
	t.Helper()
	store := sqlitetest.NewStore(t, "confirmations-web.db")
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	confirmations := confirmation.NewService(store, confirmation.Config{})
	server := web.NewServer(
		context.Background(), &fakeOrchestrator{store: store}, store, store,
		reg, policy, "campus-services", newTestHub(store, "campus-services"),
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithConfirmations(confirmations),
	)
	return server.Handler(), store, confirmations
}

func seedWaitingConfirmation(t *testing.T, store *sqlite.Store, service *confirmation.Service) (string, confirmation.Confirmation) {
	t.Helper()
	echoID := uuid.NewString()
	// 确认记录外键绑定已存在的 Echo 与 Run，先种入真实的 Echo/Run。
	now := time.Now().UTC()
	if err := store.CreateEchoRun(context.Background(), kernelecho.Record{
		ID: echoID, AppID: "campus-services", InputMessage: "你好",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: "run-1", RunGroupID: "run-1", AppID: "campus-services", EchoID: echoID,
		Attempt: 1, Status: kernelecho.RunStatusQueued,
		Model: "test-model", ModelConfigVersion: "v1", ProtocolVersion: "1.0",
		MaxSteps: 8, MaxToolCalls: 4, MaxInputTokens: 4096, MaxOutputTokens: 2048,
		MaxTotalTokens: 8192, MaxOutputBytes: 65536, MaxCostMicrousd: 0,
		ProviderTimeoutMS: 5000,
		Deadline:          now.Add(time.Hour),
		AvailableAt:       now,
		CreatedAt:         now,
		RecoverableState:  json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	record, err := service.Request(context.Background(), "campus-services", echoID, "run-1", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "test.send", TargetType: confirmation.TargetTypeCapability,
			TargetID: "test.send", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		[]byte(`{"text":"你好"}`), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return echoID, record
}

func TestWebConfirmationEndpointsRoundTrip(t *testing.T) {
	handler, store, service := newConfirmationTestServer(t)
	echoID, record := seedWaitingConfirmation(t, store, service)
	ctx := context.Background()

	// 列表端点返回活跃确认。
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/v1/echoes/"+echoID+"/confirmations", nil))
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), record.ConfirmationID) {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	// 单条查询端点返回确认状态。
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet,
		"/api/v1/echoes/"+echoID+"/confirmations/"+record.ConfirmationID, nil))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"status":"waiting"`) {
		t.Fatalf("get status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	// 决策批准：原子决策成功并返回 approved 状态；决策人来自可信登录态。
	authenticated := testAuthenticatedRequest(handler, http.MethodPost,
		"/api/v1/echoes/"+echoID+"/confirmations/"+record.ConfirmationID+"/decision",
		`{"decision":"approved"}`)
	if !strings.Contains(authenticated.Body.String(), `"confirmed_by":"web-test-subject"`) {
		t.Fatalf("decision attribution not bound to authenticated identity: %s", authenticated.Body.String())
	}
	if authenticated.Code != http.StatusOK || !strings.Contains(authenticated.Body.String(), `"status":"approved"`) {
		t.Fatalf("decision status=%d body=%s", authenticated.Code, authenticated.Body.String())
	}
	decided, err := service.Resolve(ctx, "campus-services", record.ConfirmationID)
	if err != nil || decided.Status != confirmation.StatusApproved {
		t.Fatalf("resolve after decision=%+v err=%v", decided.Status, err)
	}

	// 重复冲突决策返回稳定冲突错误。
	conflict := testAuthenticatedRequest(handler, http.MethodPost,
		"/api/v1/echoes/"+echoID+"/confirmations/"+record.ConfirmationID+"/decision",
		`{"decision":"rejected"}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "confirmation_already_decided") {
		t.Fatalf("conflict decision status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// 跨 Echo 的确认标识按不存在处理（fail-closed）。
	otherEchoID, _ := createEcho(t, handler, "另一条消息")
	crossEcho := httptest.NewRecorder()
	handler.ServeHTTP(crossEcho, httptest.NewRequest(http.MethodGet,
		"/api/v1/echoes/"+otherEchoID+"/confirmations/"+record.ConfirmationID, nil))
	if crossEcho.Code != http.StatusNotFound {
		t.Fatalf("cross echo status=%d", crossEcho.Code)
	}

	// 回归：过期记录的元数据同样不得泄露给其他 Echo——过期判定先于可见性返回，
	// 但 Echo 归属校验始终先行。
	if _, err := service.Expire(ctx, "campus-services", record.ConfirmationID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	expiredViaOtherEcho := httptest.NewRecorder()
	handler.ServeHTTP(expiredViaOtherEcho, httptest.NewRequest(http.MethodGet,
		"/api/v1/echoes/"+otherEchoID+"/confirmations/"+record.ConfirmationID, nil))
	if expiredViaOtherEcho.Code != http.StatusNotFound {
		t.Fatalf("cross echo expired status=%d body=%s", expiredViaOtherEcho.Code, expiredViaOtherEcho.Body.String())
	}
	// 归属 Echo 内查询过期确认：按 expired 状态呈现。
	expiredOwn := httptest.NewRecorder()
	handler.ServeHTTP(expiredOwn, httptest.NewRequest(http.MethodGet,
		"/api/v1/echoes/"+echoID+"/confirmations/"+record.ConfirmationID, nil))
	if expiredOwn.Code != http.StatusOK || !strings.Contains(expiredOwn.Body.String(), `"status":"expired"`) {
		t.Fatalf("expired own echo status=%d body=%s", expiredOwn.Code, expiredOwn.Body.String())
	}
}

func TestWebConfirmationDecisionValidatesInput(t *testing.T) {
	handler, store, service := newConfirmationTestServer(t)
	echoID, record := seedWaitingConfirmation(t, store, service)
	base := "/api/v1/echoes/" + echoID + "/confirmations/" + record.ConfirmationID + "/decision"

	for name, body := range map[string]string{
		"非法决策值":    `{"decision":"maybe"}`,
		"缺决策字段":    `{"confirmed_by":"user-1"}`,
		"客户端伪造决策人": `{"decision":"approved","confirmed_by":"user-1"}`,
		"非 JSON":   `not-json`,
	} {
		response := testAuthenticatedRequest(handler, http.MethodPost, base, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
}

// testAuthenticatedRequest 以已认证 Web 身份发送 JSON POST 请求
// （测试认证器对所有请求返回固定身份）。
func testAuthenticatedRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
