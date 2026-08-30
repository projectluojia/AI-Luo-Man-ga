package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestDispatcherRejectsNonProgressingCapabilityCycle(t *testing.T) {
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "cycle")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "service", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"cycle": {
				Spec: registry.CapabilitySpec{
					ID:              "cycle",
					Version:         "1.0.0",
					ServiceID:       "service",
					InputSchemaJSON: `{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					return dispatcher.InvokeCapability(ctx, request, "cycle", payload)
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := contracts.RequestContext{
		AppID: "app", EchoID: "echo", RequestID: "request", Deadline: time.Now().Add(time.Minute),
	}
	_, err := dispatcher.InvokeCapability(context.Background(), request, "cycle", json.RawMessage(`{"a":1,"b":2}`))
	if !errors.Is(err, runtime.ErrCycleDetected) {
		t.Fatalf("got %v, want ErrCycleDetected", err)
	}
}

func TestDispatcherValidatesCapabilitySchemaBeforeHandler(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "capability")
	called := false
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:              "capability",
		Version:         "1.0.0",
		ServiceID:       "service",
		InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`,
		SideEffect:      registry.SideEffectRead,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})

	_, err := dispatcher.InvokeCapability(context.Background(), validRequest(), "capability", json.RawMessage(`{"value":"wrong","secret":"must-not-pass"}`))
	if !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("got %v, want ErrSchemaValidation", err)
	}
	if called {
		t.Fatal("handler ran for a payload rejected by the registered schema")
	}
}

func TestDispatcherEnforcesAndNarrowsPermissions(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "capability")
	policy.Grant("app", "bus.read")
	policy.Grant("app", "system.admin")
	var observed []string
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:                  "capability",
		Version:             "1.0.0",
		ServiceID:           "service",
		InputSchemaJSON:     `{"type":"object","additionalProperties":false}`,
		SideEffect:          registry.SideEffectRead,
		RequiredPermissions: []string{"bus.read"},
	}, func(_ context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
		observed = append([]string(nil), request.PermissionScope...)
		return json.RawMessage(`{}`), nil
	})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})

	request := validRequest()
	request.PermissionScope = []string{"system.admin", "bus.read"}
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "capability", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("invoke with permission: %v", err)
	}
	if len(observed) != 1 || observed[0] != "bus.read" {
		t.Fatalf("handler observed non-narrowed permissions: %#v", observed)
	}

	request.PermissionScope = []string{"system.admin"}
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "capability", json.RawMessage(`{"malformed":true}`)); !errors.Is(err, registry.ErrPermissionDenied) {
		t.Fatalf("got %v, want ErrPermissionDenied before payload disclosure", err)
	}
}

func TestDispatcherProjectsClosedTargetIdentityToHandlers(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "service.capability")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	var capabilityContext, toolContext contracts.RequestContext
	if err := reg.RegisterTool(registry.ToolRegistration{
		Spec: registry.ToolSpec{
			ID: "service.tool", Version: "1.0.0",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      registry.SideEffectRead,
		},
		Handler: func(_ context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
			toolContext = request
			return json.RawMessage(`{"ok":true}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID: "service", Version: "1.0.0", ToolDependencies: []string{"service.tool"},
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"service.capability": {
				Spec: registry.CapabilitySpec{
					ID: "service.capability", Version: "1.0.0", ServiceID: "service",
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					capabilityContext = request
					return dispatcher.UseTool(ctx, request, "service", "service.tool", payload)
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.InvokeCapability(t.Context(), validRequest(), "service.capability", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if capabilityContext.TargetType != "capability" ||
		capabilityContext.CapabilityID != "service.capability" ||
		capabilityContext.ServiceID != "service" ||
		capabilityContext.ToolID != "" {
		t.Fatalf("Capability 目标上下文=%#v", capabilityContext)
	}
	if toolContext.TargetType != "tool" ||
		toolContext.CapabilityID != "" ||
		toolContext.ServiceID != "service" ||
		toolContext.ToolID != "service.tool" {
		t.Fatalf("Tool 目标上下文=%#v", toolContext)
	}
}

func TestDispatcherProjectsOnlyDeclaredImportedCapability(t *testing.T) {
	t.Parallel()
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "provider.cap")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	called := 0
	var consumerRequest, providerRequest contracts.RequestContext
	providerSchema := `{"type":"object","additionalProperties":false}`
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "provider", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"provider.cap": {Spec: registry.CapabilitySpec{ID: "provider.cap", Version: "1.0.0", ServiceID: "provider", InputSchemaJSON: providerSchema, SideEffect: registry.SideEffectRead}, Handler: func(_ context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
			called++
			providerRequest = request
			if request.PermissionScope != nil {
				t.Fatalf("unexpected permission scope: %#v", request.PermissionScope)
			}
			return json.RawMessage(`{"ok":true}`), nil
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "consumer", Version: "1.0.0", CapabilityImports: []registry.CapabilityImport{{ID: "provider.cap", Version: "1.0.0"}}},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"consumer.cap": {Spec: registry.CapabilitySpec{ID: "consumer.cap", Version: "1.0.0", ServiceID: "consumer", InputSchemaJSON: providerSchema, SideEffect: registry.SideEffectRead}, Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			consumerRequest = request
			return dispatcher.InvokeImportedCapability(ctx, request, "consumer", "provider.cap", payload)
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "other", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"other.cap": {Spec: registry.CapabilitySpec{ID: "other.cap", Version: "1.0.0", ServiceID: "other", InputSchemaJSON: providerSchema, SideEffect: registry.SideEffectRead}, Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	policy.Enable("app", "consumer.cap")
	if _, err := dispatcher.InvokeCapability(context.Background(), validRequest(), "consumer.cap", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("imported call failed: %v", err)
	}
	if called != 1 {
		t.Fatalf("provider calls = %d, want 1", called)
	}
	if len(consumerRequest.ImportedCapabilities) != 1 ||
		consumerRequest.ImportedCapabilities[0].ID != "provider.cap" ||
		consumerRequest.ImportedCapabilities[0].Version != "1.0.0" ||
		consumerRequest.ImportedCapabilities[0].InputSchemaJSON != providerSchema {
		t.Fatalf("consumer hop projection=%#v", consumerRequest.ImportedCapabilities)
	}
	if len(providerRequest.ImportedCapabilities) != 0 || providerRequest.ServiceID != "provider" ||
		providerRequest.CapabilityID != "provider.cap" {
		t.Fatalf("provider hop request=%#v", providerRequest)
	}
	request := validRequest()
	request.TargetType = registry.TargetTypeCapability
	request.ServiceID = "other"
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "provider.cap", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityNotImported) {
		t.Fatalf("undeclared call error = %v", err)
	}
}

func TestDispatcherImportEnforcementFailsClosed(t *testing.T) {
	t.Parallel()
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "provider.cap")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "provider", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"provider.cap": {Spec: registry.CapabilitySpec{ID: "provider.cap", Version: "1.0.0", ServiceID: "provider", InputSchemaJSON: `{"type":"object","additionalProperties":false}`, SideEffect: registry.SideEffectRead}, Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "consumer", Version: "1.0.0", CapabilityImports: []registry.CapabilityImport{{ID: "provider.cap", Version: "1.0.0"}}},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"consumer.cap": {Spec: registry.CapabilitySpec{ID: "consumer.cap", Version: "1.0.0", ServiceID: "consumer", InputSchemaJSON: `{"type":"object","additionalProperties":false}`, SideEffect: registry.SideEffectRead}, Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			// handler 身份已绑定为 consumer；显式声明冒充其他消费方必须被拒绝。
			return dispatcher.InvokeImportedCapability(ctx, request, "other", "provider.cap", payload)
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "other", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{"other.cap": {Spec: registry.CapabilitySpec{ID: "other.cap", Version: "1.0.0", ServiceID: "other", InputSchemaJSON: `{"type":"object","additionalProperties":false}`, SideEffect: registry.SideEffectRead}, Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			// 绑定身份与显式声明一致，但 other 未声明任何 import：
			// 必须走注册表查询并 fail-closed。
			return dispatcher.InvokeImportedCapability(ctx, request, "other", "provider.cap", payload)
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	policy.Enable("app", "consumer.cap")
	policy.Enable("app", "other.cap")
	// 未绑定身份的调用方即使声称自己是通过导入校验的 consumer 也必须拒绝。
	spoofed := validRequest()
	spoofed.TargetType = registry.TargetTypeCapability
	spoofed.ServiceID = "consumer"
	if _, err := dispatcher.InvokeCapability(context.Background(), spoofed, "provider.cap", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityNotImported) {
		t.Fatalf("impersonated import error = %v", err)
	}
	// 未绑定身份时显式导入入口必须拒绝。
	if _, err := dispatcher.InvokeImportedCapability(context.Background(), validRequest(), "consumer", "provider.cap", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityNotImported) {
		t.Fatalf("unbound imported invoke error = %v", err)
	}
	// 绑定身份与显式声明不一致必须拒绝。
	if _, err := dispatcher.InvokeCapability(context.Background(), validRequest(), "consumer.cap", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityNotImported) {
		t.Fatalf("mismatched import declaration error = %v", err)
	}
	// 绑定身份一致的调用方仍需通过注册表 import 查询。
	if _, err := dispatcher.InvokeCapability(context.Background(), validRequest(), "other.cap", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityNotImported) {
		t.Fatalf("undeclared bound import error = %v", err)
	}
}

func TestDispatcherUsesPersistentAppPolicyAndRevalidatesChanges(t *testing.T) {
	reg := registry.New()
	called := 0
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:                  "capability",
		Version:             "1.0.0",
		ServiceID:           "service",
		InputSchemaJSON:     `{"type":"object","additionalProperties":false}`,
		SideEffect:          registry.SideEffectRead,
		RequiredPermissions: []string{"bus.read"},
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		called++
		return json.RawMessage(`{}`), nil
	})
	store := openIdempotencyStore(t)
	current, _, err := store.Ensure(t.Context(), dispatcherAppConfig())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})

	request := validRequest()
	request.PermissionScope = []string{"bus.read"}
	if _, err := dispatcher.InvokeCapability(t.Context(), request, "capability", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("invoke with persisted grant: %v", err)
	}
	request.PermissionScope = []string{"bus.read", "system.admin"}
	if _, err := dispatcher.InvokeCapability(t.Context(), request, "capability", json.RawMessage(`{}`)); !errors.Is(err, registry.ErrPermissionDenied) {
		t.Fatalf("App grant upper bound error=%v", err)
	}

	replacement := current
	replacement.EnabledCapabilities = nil
	current, err = store.CompareAndSwap(t.Context(), current.Generation, replacement)
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionScope = []string{"bus.read"}
	if _, err := dispatcher.InvokeCapability(t.Context(), request, "capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityDisabled) {
		t.Fatalf("revoked capability error=%v", err)
	}

	replacement = current
	replacement.Enabled = false
	replacement.EnabledCapabilities = []string{"capability"}
	if _, err := store.CompareAndSwap(t.Context(), current.Generation, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.InvokeCapability(t.Context(), request, "capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrCapabilityDisabled) {
		t.Fatalf("disabled App error=%v", err)
	}
	if called != 1 {
		t.Fatalf("handler call count=%d, want 1", called)
	}
}

func TestDispatcherFailsClosedWhenAppPolicyIsUnavailable(t *testing.T) {
	reg := registry.New()
	called := false
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:              "capability",
		Version:         "1.0.0",
		ServiceID:       "service",
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		SideEffect:      registry.SideEffectRead,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	})
	dispatcher := runtime.NewDispatcher(reg, appPolicyFunc(func(context.Context, string) (appconfig.PolicySnapshot, error) {
		return appconfig.PolicySnapshot{}, errors.New("private storage failure")
	}), runtime.DispatcherConfig{})
	if _, err := dispatcher.InvokeCapability(t.Context(), validRequest(), "capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrAppPolicyUnavailable) {
		t.Fatalf("policy failure error=%v", err)
	}
	if called {
		t.Fatal("handler ran while App policy was unavailable")
	}
}

func TestDispatcherPreventsInternalPermissionGain(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "capability")
	policy.Grant("app", "private.read")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	if err := reg.RegisterTool(registry.ToolRegistration{
		Spec: registry.ToolSpec{
			ID:                  "private-tool",
			Version:             "1.0.0",
			InputSchemaJSON:     `{"type":"object","additionalProperties":false}`,
			SideEffect:          registry.SideEffectRead,
			RequiredPermissions: []string{"private.read"},
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:                   "service",
			Version:              "1.0.0",
			ToolDependencies:     []string{"private-tool"},
			RequestedPermissions: []string{"private.read"},
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"capability": {
				Spec: registry.CapabilitySpec{
					ID:              "capability",
					Version:         "1.0.0",
					ServiceID:       "service",
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					return dispatcher.UseTool(ctx, request, "service", "private-tool", payload)
				},
			},
		},
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	request := validRequest()
	request.PermissionScope = []string{"private.read"}
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "capability", json.RawMessage(`{}`)); !errors.Is(err, registry.ErrPermissionDenied) {
		t.Fatalf("got %v, want permission narrowing to deny internal gain", err)
	}
}

func TestDispatcherEnforcesSideEffectIdempotency(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "write-capability")
	called := 0
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:              "write-capability",
		Version:         "1.0.0",
		ServiceID:       "service",
		InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"string"}},"additionalProperties":false}`,
		SideEffect:      registry.SideEffectWrite,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		called++
		return json.RawMessage(`{}`), nil
	})
	withoutStore := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})

	request := validRequest()
	if _, err := withoutStore.InvokeCapability(context.Background(), request, "write-capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrIdempotencyKeyRequired) {
		t.Fatalf("got %v, want ErrIdempotencyKeyRequired", err)
	}
	request.IdempotencyKey = "operation-1"
	if _, err := withoutStore.InvokeCapability(context.Background(), request, "write-capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrIdempotencyUnavailable) {
		t.Fatalf("got %v, want ErrIdempotencyUnavailable", err)
	}
	store := openIdempotencyStore(t)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "write-capability", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("invoke governed write: %v", err)
	}
	request.RequestID = "request-replay"
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "write-capability", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("replay governed write: %v", err)
	}
	if called != 1 {
		t.Fatalf("handler call count = %d, want 1", called)
	}
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "write-capability", json.RawMessage(`{"value":"different"}`)); !errors.Is(err, idempotency.ErrKeyConflict) {
		t.Fatalf("conflicting key got %v, want ErrKeyConflict", err)
	}
}

func TestDispatcherRequiresGovernedConfirmation(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "external-capability")
	registerCapability(t, reg, registry.CapabilitySpec{
		ID:                   "external-capability",
		Version:              "1.0.0",
		ServiceID:            "service",
		InputSchemaJSON:      `{"type":"object","additionalProperties":false}`,
		SideEffect:           registry.SideEffectExternal,
		RequiresConfirmation: true,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})

	request := validRequest()
	request.IdempotencyKey = "operation-1"
	request.ConfirmationID = "confirmation-1"
	withoutVerifier := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	if _, err := withoutVerifier.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{}`)); !errors.Is(err, runtime.ErrConfirmationRequired) {
		t.Fatalf("got %v, want ErrConfirmationRequired", err)
	}

	var verified runtime.ConfirmationRequest
	store := openIdempotencyStore(t)
	withVerifier := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore: store,
		ConfirmationVerifier: verifierFunc(func(_ context.Context, request runtime.ConfirmationRequest) error {
			verified = request
			return nil
		}),
	})
	if _, err := withVerifier.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("invoke confirmed capability: %v", err)
	}
	if verified.AppID != "app" || verified.ConfirmationID != "confirmation-1" || verified.TargetID != "external-capability" || verified.IdempotencyKey != "operation-1" {
		t.Fatalf("unexpected confirmation request: %#v", verified)
	}
}

func TestDispatcherRevalidatesAtToolBoundary(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "capability")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	toolCalled := false
	if err := reg.RegisterTool(registry.ToolRegistration{
		Spec: registry.ToolSpec{
			ID:              "tool",
			Version:         "1.0.0",
			InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`,
			SideEffect:      registry.SideEffectRead,
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			toolCalled = true
			return json.RawMessage(`{}`), nil
		},
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "service", Version: "1.0.0", ToolDependencies: []string{"tool"}},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"capability": {
				Spec: registry.CapabilitySpec{
					ID:              "capability",
					Version:         "1.0.0",
					ServiceID:       "service",
					InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
					return dispatcher.UseTool(ctx, request, "service", "tool", payload)
				},
			},
		},
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	if _, err := dispatcher.InvokeCapability(context.Background(), validRequest(), "capability", json.RawMessage(`{"value":"not-an-integer"}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("got %v, want tool schema rejection", err)
	}
	if toolCalled {
		t.Fatal("tool handler ran after tool-boundary schema rejection")
	}
}

type verifierFunc func(context.Context, runtime.ConfirmationRequest) error

func (f verifierFunc) VerifyConfirmation(ctx context.Context, request runtime.ConfirmationRequest) error {
	return f(ctx, request)
}

type appPolicyFunc func(context.Context, string) (appconfig.PolicySnapshot, error)

func (f appPolicyFunc) Snapshot(ctx context.Context, appID string) (appconfig.PolicySnapshot, error) {
	return f(ctx, appID)
}

func registerCapability(t *testing.T, reg *registry.Registry, spec registry.CapabilitySpec, handler registry.Handler) {
	t.Helper()
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:                   spec.ServiceID,
			Version:              "1.0.0",
			RequestedPermissions: append([]string(nil), spec.RequiredPermissions...),
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			spec.ID: {Spec: spec, Handler: handler},
		},
	}); err != nil {
		t.Fatalf("register capability: %v", err)
	}
}

func validRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID:     "app",
		EchoID:    "echo",
		RequestID: "request",
		Deadline:  time.Now().Add(time.Minute),
	}
}

func openIdempotencyStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatalf("open idempotency store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close idempotency store: %v", err)
		}
	})
	return store
}

func dispatcherAppConfig() appconfig.Config {
	return appconfig.Config{
		AppID: "app", Enabled: true, Model: "test-model", SystemPrompt: "系统提示",
		Timezone: "Asia/Shanghai", MaxSteps: 8, MaxToolCalls: 8,
		MaxInputTokens: 32768, MaxOutputTokens: 8192, MaxTotalTokens: 40960,
		MaxOutputBytes: 65536, ProviderTimeout: 30 * time.Second,
		EnabledCapabilities: []string{"capability"}, PermissionScope: []string{"bus.read"},
	}
}
