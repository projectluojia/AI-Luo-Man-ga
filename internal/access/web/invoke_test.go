package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

const pingSchema = `{"type":"object","properties":{"text":{"type":"string","minLength":1}},"required":["text"],"additionalProperties":false}`

// newInvokeServer 构造带 Dispatcher 的测试 server，返回 handler 与可配置的
// registry/policy，供各用例注册 capability 与授权。
func newInvokeServer(t *testing.T) (http.Handler, *registry.Registry, *runtimetest.StaticAppPolicy) {
	return newInvokeServerWithResolver(t, testWebResolver{})
}

func newInvokeServerWithResolver(t *testing.T, resolver access.IdentityResolver) (http.Handler, *registry.Registry, *runtimetest.StaticAppPolicy) {
	t.Helper()
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	handler := web.NewServer(
		&fakeOrchestrator{}, nil, nil,
		reg, policy, "campus-services", nil, testController{}, access.NewEventHub(),
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithIdentityResolver(resolver),
		web.WithDispatcher(dispatcher),
	).Handler()
	return handler, reg, policy
}

// registerPing 注册 read 侧 echo.ping 与未启用的 echo.hidden，仅启用前者。
func registerPing(t *testing.T, reg *registry.Registry, policy *runtimetest.StaticAppPolicy) {
	t.Helper()
	err := reg.RegisterService(registry.ServiceRegistration{
		Spec: capability.ServiceSpec{ID: "echo", Version: "1.0.0", Description: "echo service"},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			"echo.ping": {
				Spec: capability.CapabilitySpec{
					ID: "echo.ping", Version: "1.0.0", ServiceID: "echo",
					Name: "echo", InputSchemaJSON: pingSchema, SideEffect: capability.SideEffectRead,
				},
				Handler: func(_ context.Context, _ contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`{"echo":` + string(payload) + `}`), nil
				},
			},
			"echo.hidden": {
				Spec: capability.CapabilitySpec{
					ID: "echo.hidden", Version: "1.0.0", ServiceID: "echo",
					Name: "hidden", InputSchemaJSON: pingSchema, SideEffect: capability.SideEffectRead,
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
	return invokeRequestWithHeaders(t, handler, capabilityID, body, nil)
}

func invokeRequestWithHeaders(t *testing.T, handler http.Handler, capabilityID, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/capabilities/"+capabilityID+"/invoke", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
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
		&fakeOrchestrator{}, nil, nil,
		reg, policy, "campus-services", nil, testController{}, access.NewEventHub(),
		web.WithDispatcher(dispatcher),
	).Handler()
	response := invokeRequest(t, server, "echo.ping", `{"input":{"text":"x"}}`)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"authentication_required"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type identityResolverFunc func(context.Context, string, string, string, string) (identity.IdentityContext, error)

func (f identityResolverFunc) ResolveIdentity(ctx context.Context, appID, platform, spaceID, userID string) (identity.IdentityContext, error) {
	return f(ctx, appID, platform, spaceID, userID)
}

func TestInvokeCapabilityRejectsUnusableResolvedIdentity(t *testing.T) {
	const appID = "campus-services"
	tests := []struct {
		name       string
		resolveErr error
		resolved   identity.IdentityContext
		status     int
		code       string
	}{
		{name: "unknown", resolveErr: identity.ErrNotFound, status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "disabled", resolveErr: identity.ErrUserDisabled, status: http.StatusForbidden, code: "permission_denied"},
		{
			name:     "no membership",
			resolved: identity.IdentityContext{AppID: appID, UserID: "internal-user"},
			status:   http.StatusForbidden,
			code:     "permission_denied",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := identityResolverFunc(func(_ context.Context, _, _, _, _ string) (identity.IdentityContext, error) {
				return test.resolved, test.resolveErr
			})
			handler, _, _ := newInvokeServerWithResolver(t, resolver)
			response := invokeRequest(t, handler, "echo.ping", `{"input":{"text":"x"}}`)
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s, want %d/%s", response.Code, response.Body.String(), test.status, test.code)
			}
		})
	}
}

type recordingIdempotencyStore struct {
	claim     idempotency.Claim
	completed bool
}

func (s *recordingIdempotencyStore) BeginIdempotent(_ context.Context, claim idempotency.Claim, _ time.Time) (idempotency.Record, bool, error) {
	s.claim = claim
	return idempotency.Record{}, true, nil
}

func (*recordingIdempotencyStore) GetIdempotent(context.Context, string, string, string) (idempotency.Record, error) {
	return idempotency.Record{}, idempotency.ErrRecordNotFound
}

func (s *recordingIdempotencyStore) CompleteIdempotent(_ context.Context, _ idempotency.Claim, _ string, _ []byte, _ string, _, _ time.Time) error {
	s.completed = true
	return nil
}

type confirmationVerifierFunc func(context.Context, runtime.ConfirmationRequest) error

func (f confirmationVerifierFunc) VerifyConfirmation(ctx context.Context, request runtime.ConfirmationRequest) error {
	return f(ctx, request)
}

func permittedIdentityResolver() access.IdentityResolver {
	return identityResolverFunc(func(_ context.Context, appID, _, _, _ string) (identity.IdentityContext, error) {
		return identity.IdentityContext{
			AppID: appID, UserID: "internal-user",
			Membership:  &identity.AppMembership{AppID: appID, UserID: "internal-user"},
			Permissions: []string{"echo.write"},
		}, nil
	})
}

func newGovernedWriteServer(t *testing.T, resolver access.IdentityResolver, store idempotency.Store, verifier runtime.ConfirmationVerifier, handler registry.Handler) http.Handler {
	t.Helper()
	const appID = "campus-services"
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: capability.ServiceSpec{ID: "echo", Version: "1.0.0", Description: "echo service", RequestedPermissions: []string{"echo.write"}},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			"echo.write": {
				Spec: capability.CapabilitySpec{
					ID: "echo.write", Version: "1.0.0", ServiceID: "echo",
					InputSchemaJSON: pingSchema, SideEffect: capability.SideEffectWrite,
					RequiresConfirmation: true, RequiredPermissions: []string{"echo.write"},
				},
				Handler: handler,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy.Enable(appID, "echo.write")
	policy.Grant(appID, "echo.write")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store, ConfirmationVerifier: verifier})
	return web.NewServer(
		&fakeOrchestrator{}, nil, nil, reg, policy, appID, nil, testController{}, access.NewEventHub(),
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithIdentityResolver(resolver),
		web.WithDispatcher(dispatcher),
	).Handler()
}

func TestInvokeCapabilityUsesResolvedIdentityAndGovernanceHeaders(t *testing.T) {
	const appID = "campus-services"
	var observed contracts.RequestContext
	var confirmation runtime.ConfirmationRequest
	store := &recordingIdempotencyStore{}
	handler := newGovernedWriteServer(t, permittedIdentityResolver(), store,
		confirmationVerifierFunc(func(_ context.Context, request runtime.ConfirmationRequest) error {
			confirmation = request
			return nil
		}),
		func(_ context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
			observed = request
			return json.RawMessage(`{"ok":true}`), nil
		})
	response := invokeRequestWithHeaders(t, handler, "echo.write", `{"input":{"text":"x"}}`, map[string]string{
		"Idempotency-Key":   "operation-1",
		"X-Confirmation-ID": "confirmation-1",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if observed.UserID != "internal-user" || observed.SessionID != "web-test-session" ||
		len(observed.PermissionScope) != 1 || observed.PermissionScope[0] != "echo.write" {
		t.Fatalf("governed context = %#v", observed)
	}
	if store.claim.Key != "operation-1" || !store.completed {
		t.Fatalf("idempotency operation = %#v completed=%t", store.claim.Operation, store.completed)
	}
	if confirmation.ConfirmationID != "confirmation-1" || confirmation.IdempotencyKey != "operation-1" ||
		confirmation.TargetID != "echo.write" || confirmation.AppID != appID {
		t.Fatalf("confirmation request = %#v", confirmation)
	}
}

func TestInvokeCapabilityRejectsInvalidIdempotencyKey(t *testing.T) {
	store := &recordingIdempotencyStore{}
	handler := newGovernedWriteServer(t, permittedIdentityResolver(), store,
		confirmationVerifierFunc(func(context.Context, runtime.ConfirmationRequest) error { return nil }),
		func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		})
	response := invokeRequestWithHeaders(t, handler, "echo.write", `{"input":{"text":"x"}}`, map[string]string{
		"Idempotency-Key":   "contains space",
		"X-Confirmation-ID": "confirmation-1",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_idempotency_key"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
