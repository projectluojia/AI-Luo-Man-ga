package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
)

func TestDispatcherAuthorizesConcreteResource(t *testing.T) {
	reg := registry.New()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "library.book.get", Version: "1.0.0", Name: "查看图书",
			InputSchemaJSON: `{"type":"object","required":["book_id"],"additionalProperties":false,"properties":{"book_id":{"type":"string"}}}`,
			Authorization:   capability.AuthorizationSpec{ResourceType: "library.book", ResourceIDFrom: "/book_id"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.EnableResource("app", "library.book.get", "library.book", []string{"book-1"})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	request := contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request", UserID: "alice", Deadline: time.Now().Add(time.Minute)}
	if _, err := dispatcher.InvokeCapability(t.Context(), request, "library.book.get", []byte(`{"book_id":"book-1"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := dispatcher.InvokeCapability(t.Context(), request, "library.book.get", []byte(`{"book_id":"book-2"}`))
	if !errors.Is(err, runtime.ErrAuthorizationDenied) {
		t.Fatalf("unauthorized resource error=%v", err)
	}
}

// TestDispatcherEnforcesRunFrozenScope 验证 Run 介导的调用只能触达 Run 接受时
// 冻结投影内的 Capability：范围内放行，范围外 fail-closed；Web 直连（无 RunID）
// 不受 Run 范围约束，仍按 App 策略授权。
func TestDispatcherEnforcesRunFrozenScope(t *testing.T) {
	reg := registry.New()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "library.book.get", Version: "1.0.0", Name: "查看图书",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "library.book.get")
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	deadline := time.Now().Add(time.Minute)
	within := contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request",
		RunID: "run-1", RunCapabilityGrants: []capability.Grant{{CapabilityID: "library.book.get"}},
		Deadline: deadline}
	if _, err := dispatcher.InvokeCapability(t.Context(), within, "library.book.get", []byte(`{}`)); err != nil {
		t.Fatalf("in-scope capability error=%v", err)
	}
	outside := contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request",
		RunID: "run-1", Deadline: deadline}
	if _, err := dispatcher.InvokeCapability(t.Context(), outside, "library.book.get", []byte(`{}`)); !errors.Is(err, runtime.ErrCapabilityDisabled) {
		t.Fatalf("out-of-scope capability error=%v, want ErrCapabilityDisabled", err)
	}
	direct := contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request", Deadline: deadline}
	if _, err := dispatcher.InvokeCapability(t.Context(), direct, "library.book.get", []byte(`{}`)); err != nil {
		t.Fatalf("web-direct capability error=%v", err)
	}
}

// TestDispatcherValidatesSchemaBeforeAuthorization 验证 Schema 校验先于授权：
// 载荷畸形时即使 Grant 也不匹配，也必须返回 Schema 校验错误（HTTP 400
// invalid_input），而不是被授权错误吞成 permission_denied（403）。
func TestDispatcherValidatesSchemaBeforeAuthorization(t *testing.T) {
	reg := registry.New()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "library.book.get", Version: "1.0.0", Name: "查看图书",
			InputSchemaJSON: `{"type":"object","required":["book_id"],"additionalProperties":false,"properties":{"book_id":{"type":"string"}}}`,
			Authorization:   capability.AuthorizationSpec{ResourceType: "library.book", ResourceIDFrom: "/book_id"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	// 故意不授权该 Capability：授权必然拒绝，但畸形载荷应先被 Schema 拦下。
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	request := contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request", UserID: "alice", Deadline: time.Now().Add(time.Minute)}
	_, err := dispatcher.InvokeCapability(t.Context(), request, "library.book.get", []byte(`{"book_id":123}`))
	if !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("malformed payload error=%v, want ErrSchemaValidation", err)
	}
}
