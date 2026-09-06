package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestRegisterBatchPublishesAllOrNothing(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	valid := registration("capability.a", strictEmptySchema)
	invalid := registration("capability.b", `{"type":"object"}`)
	if err := reg.RegisterBatch([]registry.CapabilityRegistration{valid, invalid}); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("批量注册错误=%v，期望 ErrInvalidSpec", err)
	}
	if len(reg.Capabilities()) != 0 {
		t.Fatalf("失败批次污染 Registry：%#v", reg.Capabilities())
	}

	if err := reg.RegisterBatch([]registry.CapabilityRegistration{valid}); err != nil {
		t.Fatalf("注册有效批次：%v", err)
	}
	if got := len(reg.Capabilities()); got != 1 {
		t.Fatalf("能力数量=%d，期望 1", got)
	}

	conflict := registration("capability.a", strictEmptySchema)
	if err := reg.RegisterBatch([]registry.CapabilityRegistration{registration("capability.b", strictEmptySchema), conflict}); !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("冲突批次错误=%v，期望 ErrDuplicateID", err)
	}
	if got := len(reg.Capabilities()); got != 1 {
		t.Fatalf("冲突批次部分提交，能力数量=%d", got)
	}
}

func TestRegisterRejectsInvalidCapabilityContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec capability.CapabilitySpec
	}{
		{name: "invalid id", spec: capability.CapabilitySpec{ID: "Capability A", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead}},
		{name: "invalid semantic version", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0-01", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead}},
		{name: "unknown side effect", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: "maybe"}},
		{name: "read confirmation", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead, RequiresConfirmation: true}},
		{name: "missing schema", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", SideEffect: capability.SideEffectRead}},
		{name: "permissive root object", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`, SideEffect: capability.SideEffectRead}},
		{name: "permissive nested object", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"nested":{"type":"object"}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead}},
		{name: "implicit object type", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"nested":{"properties":{"value":{"type":"string"}},"additionalProperties":false}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead}},
		{name: "external reference", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"value":{"$ref":"https://example.invalid/schema"}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead}},
		{name: "duplicate schema key", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","type":"object","additionalProperties":false}`, SideEffect: capability.SideEffectRead}},
		{name: "duplicate permission", spec: capability.CapabilitySpec{ID: "capability.a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead, RequiredPermissions: []string{"bus.read", "bus.read"}}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reg := registry.New()
			if err := reg.Register(registry.CapabilityRegistration{Spec: test.spec, Handler: noopHandler}); !errors.Is(err, registry.ErrInvalidSpec) {
				t.Fatalf("错误=%v，期望 ErrInvalidSpec", err)
			}
			if len(reg.Capabilities()) != 0 {
				t.Fatal("失败注册改变了 Registry")
			}
		})
	}
}

func TestValidateCapabilityInputUsesCompiledStrictSchema(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "capability.a", Version: "1.0.0",
			InputSchemaJSON: `{"type":"object","properties":{"count":{"type":"integer","minimum":1},"at":{"type":"string","format":"date-time"}},"required":["count","at"],"additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		},
		Handler: noopHandler,
	}); err != nil {
		t.Fatalf("注册能力：%v", err)
	}

	if err := reg.ValidateCapabilityInput("capability.a", json.RawMessage(`{"count":1,"at":"2026-07-26T12:00:00+08:00"}`)); err != nil {
		t.Fatalf("有效输入校验：%v", err)
	}
	invalid := []json.RawMessage{
		json.RawMessage(`{"count":0,"at":"2026-07-26T12:00:00+08:00"}`),
		json.RawMessage(`{"count":1,"at":"not-a-time"}`),
		json.RawMessage(`{"count":1}`),
		json.RawMessage(`{"count":1,"at":"2026-07-26T12:00:00+08:00","extra":true}`),
		json.RawMessage(`{"count":1,"count":2,"at":"2026-07-26T12:00:00+08:00"}`),
		json.RawMessage(`{"count":1,"at":"2026-07-26T12:00:00+08:00"} {}`),
		json.RawMessage(strings.Repeat(" ", 64<<10) + `{}`),
	}
	for _, payload := range invalid {
		if err := reg.ValidateCapabilityInput("capability.a", payload); !errors.Is(err, registry.ErrSchemaValidation) {
			t.Fatalf("输入 %q 错误=%v，期望 ErrSchemaValidation", payload, err)
		}
	}
}

func TestRegistryMetadataSnapshotsAreImmutable(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "capability.a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema,
			SideEffect: capability.SideEffectRead, RequiredPermissions: []string{"bus.read"},
		},
		Handler: noopHandler,
	}); err != nil {
		t.Fatalf("注册能力：%v", err)
	}

	capabilities := reg.Capabilities()
	capabilities[0].RequiredPermissions[0] = "mutated"
	resolved, _, err := reg.ResolveCapability("capability.a")
	if err != nil {
		t.Fatalf("解析能力：%v", err)
	}
	if resolved.RequiredPermissions[0] != "bus.read" {
		t.Fatalf("Registry 元数据被外部切片修改：%#v", resolved)
	}
}

const strictEmptySchema = `{"type":"object","additionalProperties":false}`

var noopHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func registration(id, schema string) registry.CapabilityRegistration {
	return registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: id, Version: "1.0.0", Name: id, Description: "test capability",
			InputSchemaJSON: schema, SideEffect: capability.SideEffectRead,
		},
		Handler: noopHandler,
	}
}
