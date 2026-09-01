package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestRegisterServiceRequiresDeclaredTools(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	err := reg.RegisterService(serviceRegistration("service-a", "capability-a", "missing-tool"))
	if !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("got error %v, want ErrToolNotFound", err)
	}
	if len(reg.Services()) != 0 || len(reg.Capabilities()) != 0 {
		t.Fatal("failed service registration changed registry state")
	}
}

func TestRegisterServiceIsAtomicOnDuplicateCapability(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "tool-a")
	if err := reg.RegisterService(serviceRegistration("service-a", "shared-capability", "tool-a")); err != nil {
		t.Fatalf("register first service: %v", err)
	}
	err := reg.RegisterService(serviceRegistration("service-b", "shared-capability", "tool-a"))
	if !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("got error %v, want ErrDuplicateID", err)
	}
	if len(reg.Services()) != 1 {
		t.Fatalf("got %d services after failed registration, want 1", len(reg.Services()))
	}
}

func TestRegisterBatchPublishesAllOrNothing(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	tool := registry.ToolRegistration{
		Spec: capability.ToolSpec{
			ID: "tool-a", Version: "1.0.0",
			InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead,
		},
		Handler: noopHandler,
	}
	valid := serviceRegistration("service-a", "capability-a", "tool-a")
	invalid := serviceRegistration("service-b", "capability-b", "missing-tool")
	if err := reg.RegisterBatch([]registry.ToolRegistration{tool}, []registry.ServiceRegistration{valid, invalid}); !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("批量注册错误=%v", err)
	}
	if len(reg.Tools()) != 0 || len(reg.Services()) != 0 || len(reg.Capabilities()) != 0 {
		t.Fatalf("失败批次污染 Registry：tools=%#v services=%#v capabilities=%#v",
			reg.Tools(), reg.Services(), reg.Capabilities())
	}
	if err := reg.RegisterBatch([]registry.ToolRegistration{tool}, []registry.ServiceRegistration{valid}); err != nil {
		t.Fatal(err)
	}
	if len(reg.Tools()) != 1 || len(reg.Services()) != 1 || len(reg.Capabilities()) != 1 {
		t.Fatalf("成功批次不完整：tools=%#v services=%#v capabilities=%#v",
			reg.Tools(), reg.Services(), reg.Capabilities())
	}

	conflictingTool := tool
	conflictingTool.Spec.ID = "tool-b"
	conflictingService := serviceRegistration("service-c", "capability-a", "tool-b")
	if err := reg.RegisterBatch([]registry.ToolRegistration{conflictingTool}, []registry.ServiceRegistration{conflictingService}); !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("冲突批次错误=%v", err)
	}
	if len(reg.Tools()) != 1 || len(reg.Services()) != 1 {
		t.Fatalf("冲突批次部分提交：tools=%#v services=%#v", reg.Tools(), reg.Services())
	}
}

func TestResolveToolRejectsUndeclaredDependency(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "tool-declared")
	registerTool(t, reg, "tool-private")
	if err := reg.RegisterService(serviceRegistration("service-a", "capability-a", "tool-declared")); err != nil {
		t.Fatalf("register service: %v", err)
	}
	_, _, err := reg.ResolveTool("service-a", "tool-private")
	if !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("got error %v, want ErrToolNotFound", err)
	}
}

// TestSharedToolResolvesForEveryDeclaringService 验证共享契约：Tool 是全局目录，
// 任何声明 ToolDependencies 的服务都解析到同一个 handler；未声明依赖的服务被拒绝。
func TestSharedToolResolvesForEveryDeclaringService(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "shared.tool")
	if err := reg.RegisterService(serviceRegistration("service-a", "capability-a", "shared.tool")); err != nil {
		t.Fatalf("register service-a: %v", err)
	}
	if err := reg.RegisterService(serviceRegistration("service-b", "capability-b", "shared.tool")); err != nil {
		t.Fatalf("register service-b: %v", err)
	}
	firstSpec, firstHandler, err := reg.ResolveTool("service-a", "shared.tool")
	if err != nil {
		t.Fatalf("resolve via service-a: %v", err)
	}
	secondSpec, secondHandler, err := reg.ResolveTool("service-b", "shared.tool")
	if err != nil {
		t.Fatalf("resolve via service-b: %v", err)
	}
	if firstSpec.ID != secondSpec.ID ||
		reflect.ValueOf(firstHandler).Pointer() != reflect.ValueOf(secondHandler).Pointer() {
		t.Fatalf("services resolved different tool handlers: %p vs %p", firstHandler, secondHandler)
	}
	// 已注册但未声明依赖的服务被拒绝（未注册的服务在服务查找层就返回 ErrServiceNotFound）。
	registerTool(t, reg, "other.tool")
	if err := reg.RegisterService(serviceRegistration("service-c", "capability-c", "other.tool")); err != nil {
		t.Fatalf("register service-c: %v", err)
	}
	if _, _, err := reg.ResolveTool("service-c", "shared.tool"); !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("undeclared service error=%v, want ErrToolNotFound", err)
	}
}

func TestRegisterToolRejectsInvalidContracts(t *testing.T) {
	t.Parallel()

	handler := func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	}
	tests := []struct {
		name string
		spec capability.ToolSpec
	}{
		{
			name: "invalid id",
			spec: capability.ToolSpec{ID: "Tool A", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead},
		},
		{
			name: "invalid semantic version",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0-01", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead},
		},
		{
			name: "unknown side effect",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: "maybe"},
		},
		{
			name: "read confirmation",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead, RequiresConfirmation: true},
		},
		{
			name: "missing schema",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", SideEffect: capability.SideEffectRead},
		},
		{
			name: "permissive root object",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`, SideEffect: capability.SideEffectRead},
		},
		{
			name: "permissive nested object",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"nested":{"type":"object"}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead},
		},
		{
			name: "implicit object type",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"nested":{"properties":{"value":{"type":"string"}},"additionalProperties":false}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead},
		},
		{
			name: "external reference",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","properties":{"value":{"$ref":"https://example.invalid/schema"}},"additionalProperties":false}`, SideEffect: capability.SideEffectRead},
		},
		{
			name: "duplicate schema key",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: `{"type":"object","type":"object","additionalProperties":false}`, SideEffect: capability.SideEffectRead},
		},
		{
			name: "duplicate permission",
			spec: capability.ToolSpec{ID: "tool-a", Version: "1.0.0", InputSchemaJSON: strictEmptySchema, SideEffect: capability.SideEffectRead, RequiredPermissions: []string{"bus.read", "bus.read"}},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reg := registry.New()
			if err := reg.RegisterTool(registry.ToolRegistration{Spec: test.spec, Handler: handler}); !errors.Is(err, registry.ErrInvalidSpec) {
				t.Fatalf("got error %v, want ErrInvalidSpec", err)
			}
			if len(reg.Tools()) != 0 {
				t.Fatal("failed tool registration changed registry state")
			}
		})
	}
}

func TestRegisterServiceCompilesAllSchemasBeforeAtomicCommit(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "tool-a")
	registration := serviceRegistration("service-a", "capability-a", "tool-a")
	registration.Capabilities["capability-b"] = struct {
		Spec    capability.CapabilitySpec
		Handler registry.Handler
	}{
		Spec: capability.CapabilitySpec{
			ID:              "capability-b",
			Version:         "1.0.0",
			ServiceID:       "service-a",
			InputSchemaJSON: `{"type":"object"}`,
			SideEffect:      capability.SideEffectRead,
		},
		Handler: registration.Capabilities["capability-a"].Handler,
	}
	if err := reg.RegisterService(registration); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("got error %v, want ErrInvalidSpec", err)
	}
	if len(reg.Services()) != 0 || len(reg.Capabilities()) != 0 {
		t.Fatal("failed schema compilation partially registered a service")
	}
}

func TestValidateCapabilityInputUsesCompiledStrictSchema(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "tool-a")
	registration := serviceRegistration("service-a", "capability-a", "tool-a")
	capability := registration.Capabilities["capability-a"]
	capability.Spec.InputSchemaJSON = `{"type":"object","properties":{"count":{"type":"integer","minimum":1},"at":{"type":"string","format":"date-time"}},"required":["count","at"],"additionalProperties":false}`
	registration.Capabilities["capability-a"] = capability
	if err := reg.RegisterService(registration); err != nil {
		t.Fatalf("register service: %v", err)
	}

	if err := reg.ValidateCapabilityInput("capability-a", json.RawMessage(`{"count":1,"at":"2026-07-26T12:00:00+08:00"}`)); err != nil {
		t.Fatalf("validate correct payload: %v", err)
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
		if err := reg.ValidateCapabilityInput("capability-a", payload); !errors.Is(err, registry.ErrSchemaValidation) {
			t.Fatalf("payload %q got error %v, want ErrSchemaValidation", payload, err)
		}
	}
}

func TestRegisterServiceEnforcesPermissionEnvelope(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	if err := reg.RegisterTool(registry.ToolRegistration{
		Spec: capability.ToolSpec{
			ID:                  "tool-a",
			Version:             "1.0.0",
			InputSchemaJSON:     strictEmptySchema,
			SideEffect:          capability.SideEffectRead,
			RequiredPermissions: []string{"bus.read"},
		},
		Handler: noopHandler,
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	registration := serviceRegistration("service-a", "capability-a", "tool-a")
	if err := reg.RegisterService(registration); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("tool permission escape got error %v, want ErrInvalidSpec", err)
	}
	if len(reg.Services()) != 0 {
		t.Fatal("failed permission validation registered a service")
	}

	registration.Spec.RequestedPermissions = []string{"bus.read"}
	capability := registration.Capabilities["capability-a"]
	capability.Spec.RequiredPermissions = []string{"bus.write"}
	registration.Capabilities["capability-a"] = capability
	if err := reg.RegisterService(registration); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("capability permission escape got error %v, want ErrInvalidSpec", err)
	}
	if len(reg.Services()) != 0 {
		t.Fatal("failed capability permission validation registered a service")
	}
}

func TestRegistryMetadataSnapshotsAreImmutable(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	registerTool(t, reg, "tool-a")
	registration := serviceRegistration("service-a", "capability-a", "tool-a")
	registration.Spec.RequestedPermissions = []string{"bus.read"}
	capability := registration.Capabilities["capability-a"]
	capability.Spec.RequiredPermissions = []string{"bus.read"}
	registration.Capabilities["capability-a"] = capability
	if err := reg.RegisterService(registration); err != nil {
		t.Fatalf("register service: %v", err)
	}

	services := reg.Services()
	services[0].ToolDependencies[0] = "mutated"
	services[0].RequestedPermissions[0] = "mutated"
	capabilities := reg.Capabilities()
	capabilities[0].RequiredPermissions[0] = "mutated"
	resolved, _, err := reg.ResolveCapability("capability-a")
	if err != nil {
		t.Fatalf("resolve capability: %v", err)
	}
	if resolved.RequiredPermissions[0] != "bus.read" {
		t.Fatalf("registry capability metadata was mutated: %#v", resolved)
	}
	if _, _, err := reg.ResolveTool("service-a", "tool-a"); err != nil {
		t.Fatalf("registry service metadata was mutated: %v", err)
	}
}

func TestRegisterBatchBindsCapabilityImportsAndProjectsProviderSpec(t *testing.T) {
	t.Parallel()

	provider := serviceRegistration("provider", "provider.capability", "")
	provider.Spec.ToolDependencies = nil
	provider.Spec.RequestedPermissions = []string{"bus.read"}
	providerCapability := provider.Capabilities["provider.capability"]
	providerCapability.Spec.RequiredPermissions = []string{"bus.read"}
	provider.Capabilities["provider.capability"] = providerCapability

	consumer := serviceRegistration("consumer", "consumer.capability", "")
	consumer.Spec.ToolDependencies = nil
	consumer.Spec.RequestedPermissions = []string{"bus.read"}
	consumer.Spec.CapabilityImports = []capability.CapabilityImport{{ID: "provider.capability", Version: "1.0.0"}}

	reg := registry.New()
	if err := reg.RegisterBatch(nil, []registry.ServiceRegistration{consumer, provider}); err != nil {
		t.Fatalf("RegisterBatch: %v", err)
	}
	ok, err := reg.IsCapabilityImported("consumer", "provider.capability")
	if err != nil || !ok {
		t.Fatalf("import lookup = %v, %v", ok, err)
	}
	projection, err := reg.ImportedCapabilityProjection("consumer")
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if len(projection) != 1 || projection[0].ID != "provider.capability" ||
		projection[0].Version != "1.0.0" || projection[0].InputSchemaJSON != strictEmptySchema ||
		!reflect.DeepEqual(projection[0].RequiredPermissions, []string{"bus.read"}) {
		t.Fatalf("projection = %#v", projection)
	}
	projection[0].RequiredPermissions[0] = "mutated"
	again, err := reg.ImportedCapabilityProjection("consumer")
	if err != nil || again[0].RequiredPermissions[0] != "bus.read" {
		t.Fatalf("projection snapshot was mutated: %#v, %v", again, err)
	}
}

func TestCapabilityImportsRequireExactVersionAndPermissionSubset(t *testing.T) {
	t.Parallel()

	provider := serviceRegistration("provider", "provider.capability", "")
	provider.Spec.ToolDependencies = nil
	provider.Spec.RequestedPermissions = []string{"bus.read", "bus.admin"}
	providerCapability := provider.Capabilities["provider.capability"]
	providerCapability.Spec.RequiredPermissions = []string{"bus.read", "bus.admin"}
	provider.Capabilities["provider.capability"] = providerCapability

	for _, test := range []struct {
		name        string
		version     string
		permissions []string
	}{
		{name: "version mismatch", version: "2.0.0", permissions: []string{"bus.read", "bus.admin"}},
		{name: "permission escalation", version: "1.0.0", permissions: []string{"bus.read"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := registry.New()
			if err := reg.RegisterService(provider); err != nil {
				t.Fatalf("register provider: %v", err)
			}
			consumer := serviceRegistration("consumer", "consumer.capability", "")
			consumer.Spec.ToolDependencies = nil
			consumer.Spec.RequestedPermissions = test.permissions
			consumer.Spec.CapabilityImports = []capability.CapabilityImport{{ID: "provider.capability", Version: test.version}}
			if err := reg.RegisterService(consumer); !errors.Is(err, registry.ErrInvalidSpec) {
				t.Fatalf("RegisterService = %v, want ErrInvalidSpec", err)
			}
			if len(reg.Services()) != 1 {
				t.Fatalf("failed import registration changed service state: %#v", reg.Services())
			}
		})
	}
}

const strictEmptySchema = `{"type":"object","additionalProperties":false}`

var noopHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func registerTool(t *testing.T, reg *registry.Registry, id string) {
	t.Helper()
	if err := reg.RegisterTool(registry.ToolRegistration{
		Spec: capability.ToolSpec{
			ID:              id,
			Version:         "1.0.0",
			InputSchemaJSON: strictEmptySchema,
			SideEffect:      capability.SideEffectRead,
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			return nil, nil
		},
	}); err != nil {
		t.Fatalf("register tool %q: %v", id, err)
	}
}

func serviceRegistration(serviceID, capabilityID, toolID string) registry.ServiceRegistration {
	handler := func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	}
	return registry.ServiceRegistration{
		Spec: capability.ServiceSpec{
			ID:               serviceID,
			Version:          "1.0.0",
			ToolDependencies: []string{toolID},
		},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			capabilityID: {
				Spec: capability.CapabilitySpec{
					ID:              capabilityID,
					Version:         "1.0.0",
					ServiceID:       serviceID,
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      capability.SideEffectRead,
				},
				Handler: handler,
			},
		},
	}
}
