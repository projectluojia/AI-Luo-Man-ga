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

const strictEmptySchema = `{"type":"object","additionalProperties":false}`

var noopHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func spec(id, schema string) capability.CapabilitySpec {
	return capability.CapabilitySpec{
		ID: id, Version: "1.0.0", Name: id, Description: "test capability", InputSchemaJSON: schema,
		Authorization: capability.AuthorizationSpec{ResourceType: "capability.resource"},
		Execution:     capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
	}
}

func registration(id, schema string) registry.CapabilityRegistration {
	return registry.CapabilityRegistration{Spec: spec(id, schema), Handler: noopHandler}
}

func TestRegisterBatchPublishesAllOrNothing(t *testing.T) {
	reg := registry.New()
	valid := registration("capability.a", strictEmptySchema)
	invalid := registration("capability.b", `{"type":"object"}`)
	if err := reg.RegisterBatch([]registry.CapabilityRegistration{valid, invalid}); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("batch error=%v", err)
	}
	if len(reg.Capabilities()) != 0 {
		t.Fatal("failed batch changed registry")
	}
	if err := reg.Register(valid); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterBatch([]registry.CapabilityRegistration{registration("capability.b", strictEmptySchema), valid}); !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestRegisterRejectsInvalidAuthorizationAndExecution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*capability.CapabilitySpec)
	}{
		{"invalid resource type", func(s *capability.CapabilitySpec) { s.Authorization.ResourceType = "bad type" }},
		{"missing resource type", func(s *capability.CapabilitySpec) { s.Authorization.ResourceType = "" }},
		{"invalid resource pointer", func(s *capability.CapabilitySpec) { s.Authorization.ResourceIDFrom = "book_id" }},
		{"invalid replay", func(s *capability.CapabilitySpec) { s.Execution.Replay = "maybe" }},
		{"invalid effect", func(s *capability.CapabilitySpec) { s.Execution.EffectTarget = "maybe" }},
		{"missing replay", func(s *capability.CapabilitySpec) { s.Execution.Replay = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := spec("capability.a", strictEmptySchema)
			test.mutate(&candidate)
			if err := registry.New().Register(registry.CapabilityRegistration{Spec: candidate, Handler: noopHandler}); !errors.Is(err, registry.ErrInvalidSpec) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestValidateCapabilityInputUsesCompiledStrictSchema(t *testing.T) {
	reg := registry.New()
	candidate := spec("capability.a", `{"type":"object","properties":{"count":{"type":"integer","minimum":1},"at":{"type":"string","format":"date-time"}},"required":["count","at"],"additionalProperties":false}`)
	if err := reg.Register(registry.CapabilityRegistration{Spec: candidate, Handler: noopHandler}); err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateCapabilityInput("capability.a", json.RawMessage(`{"count":1,"at":"2026-07-26T12:00:00+08:00"}`)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []json.RawMessage{
		[]byte(`{"count":0,"at":"2026-07-26T12:00:00+08:00"}`),
		[]byte(`{"count":1}`), []byte(`{"count":1,"at":"bad"}`),
		[]byte(`{"count":1,"at":"2026-07-26T12:00:00+08:00","extra":true}`),
		[]byte(strings.Repeat(" ", 64<<10) + `{}`),
	} {
		if err := reg.ValidateCapabilityInput("capability.a", payload); !errors.Is(err, registry.ErrSchemaValidation) {
			t.Fatalf("payload %q error=%v", payload, err)
		}
	}
}

func TestRegistryMetadataSnapshotsAreImmutable(t *testing.T) {
	reg := registry.New()
	candidate := spec("capability.a", strictEmptySchema)
	candidate.Authorization.ResourceIDFrom = "/book_id"
	if err := reg.Register(registry.CapabilityRegistration{Spec: candidate, Handler: noopHandler}); err != nil {
		t.Fatal(err)
	}
	capabilities := reg.Capabilities()
	capabilities[0].Authorization.ResourceIDFrom = "/mutated"
	resolved, _, err := reg.ResolveCapability("capability.a")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Authorization.ResourceIDFrom != "/book_id" {
		t.Fatalf("metadata changed: %#v", resolved)
	}
}
