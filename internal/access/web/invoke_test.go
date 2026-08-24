package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
)

const pingSchema = `{"type":"object","properties":{"text":{"type":"string","minLength":1}},"required":["text"],"additionalProperties":false}`

// newInvokeServer 构造带 Dispatcher 的测试 server，返回 handler 与可配置的
// registry/policy，供各用例注册 capability 与授权。
func newInvokeServer(t *testing.T) (http.Handler, *registry.Registry, *runtimetest.StaticAppPolicy) {
	t.Helper()
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	handler := web.NewServer(
		context.Background(), &fakeOrchestrator{}, nil, nil,
		reg, policy, "campus-services", nil,
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithDispatcher(dispatcher),
	).Handler()
	return handler, reg, policy
}

// registerPing 注册 read 侧 echo.ping 与未启用的 echo.hidden，仅启用前者。
func registerPing(t *testing.T, reg *registry.Registry, policy *runtimetest.StaticAppPolicy) {
	t.Helper()
	err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "echo", Version: "1.0.0", Description: "echo service"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"echo.ping": {
				Spec: registry.CapabilitySpec{
					ID: "echo.ping", Version: "1.0.0", ServiceID: "echo",
					Name: "echo", InputSchemaJSON: pingSchema, SideEffect: registry.SideEffectRead,
				},
				Handler: func(_ context.Context, _ contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`{"echo":` + string(payload) + `}`), nil
				},
			},
			"echo.hidden": {
				Spec: registry.CapabilitySpec{
					ID: "echo.hidden", Version: "1.0.0", ServiceID: "echo",
					Name: "hidden", InputSchemaJSON: pingSchema, SideEffect: registry.SideEffectRead,
				},
				Handler: func(_ context.Context, _ contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`{"ok":true}`), nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	policy.Enable("campus-services", "echo.ping")
}

func invokeRequest(t *testing.T, handler http.Handler, capabilityID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/"+capabilityID+"/invoke", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestInvokeCapabilityRoundTrip(t *testing.T) {
	handler, reg, policy := newInvokeServer(t)
	registerPing(t, reg, policy)
	response := invokeRequest(t, handler, "echo.ping", `{"input":{"text":"hello"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["capability_id"]) != `"echo.ping"` {
		t.Fatalf("capability_id = %s", body["capability_id"])
	}
	if string(body["result"]) != `{"echo":{"text":"hello"}}` {
		t.Fatalf("result = %s", body["result"])
	}
}

func TestInvokeCapabilityNotFound(t *testing.T) {
	handler, reg, policy := newInvokeServer(t)
	registerPing(t, reg, policy)
	response := invokeRequest(t, handler, "echo.missing", `{"input":{"text":"x"}}`)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"capability_not_found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInvokeCapabilityDisabled(t *testing.T) {
	handler, reg, policy := newInvokeServer(t)
	registerPing(t, reg, policy)
	// echo.hidden 已注册但未启用，必须返回 404（不泄露存在性）。
	response := invokeRequest(t, handler, "echo.hidden", `{"input":{"text":"x"}}`)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"capability_not_found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInvokeCapabilityInvalidInput(t *testing.T) {
	handler, reg, policy := newInvokeServer(t)
	registerPing(t, reg, policy)
	response := invokeRequest(t, handler, "echo.ping", `{"input":{"text":""}}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_input"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInvokeCapabilityRejectsMalformedBody(t *testing.T) {
	handler, reg, policy := newInvokeServer(t)
	registerPing(t, reg, policy)
	tests := []string{
		``,
		`{}`,
		`{"input":null}`,
		`{"input":{"text":"x"},"extra":1}`,
		`{"input":{"text":"x"}} trailing`,
	}
	for _, body := range tests {
		response := invokeRequest(t, handler, "echo.ping", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body=%q status=%d body=%s, want 400", body, response.Code, response.Body.String())
		}
	}
}

func TestInvokeCapabilityRequiresAuthentication(t *testing.T) {
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	server := web.NewServer(
		context.Background(), &fakeOrchestrator{}, nil, nil,
		reg, policy, "campus-services", nil,
		web.WithDispatcher(dispatcher),
	).Handler()
	response := invokeRequest(t, server, "echo.ping", `{"input":{"text":"x"}}`)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"authentication_required"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
