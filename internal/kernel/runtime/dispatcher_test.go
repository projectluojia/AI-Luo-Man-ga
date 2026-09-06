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
