package contracts_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

func validContext() contracts.RequestContext {
	return contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request"}
}

func TestValidateRejectsMalformedCapabilityProjections(t *testing.T) {
	t.Parallel()
	valid := contracts.CapabilityProjection{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}
	if err := validContext().Validate(time.Now()); err != nil {
		t.Fatalf("baseline context rejected: %v", err)
	}
	request := validContext()
	request.ImportedCapabilities = []contracts.CapabilityProjection{valid, {ID: "other.cap", Version: "2.0.0", InputSchemaJSON: `{"type":"string"}`}}
	if err := request.Validate(time.Now()); err != nil {
		t.Fatalf("valid projections rejected: %v", err)
	}
	tests := []struct {
		name        string
		projections []contracts.CapabilityProjection
	}{
		{name: "缺失 ID", projections: []contracts.CapabilityProjection{{Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}}},
		{name: "缺失版本", projections: []contracts.CapabilityProjection{{ID: "provider.cap", InputSchemaJSON: `{"type":"object"}`}}},
		{name: "缺失 Schema", projections: []contracts.CapabilityProjection{{ID: "provider.cap", Version: "1.0.0"}}},
		{name: "Schema 非法 JSON", projections: []contracts.CapabilityProjection{{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":`}}},
		{name: "重复 ID", projections: []contracts.CapabilityProjection{valid, valid}},
	}
	for _, test := range tests {
		request := validContext()
		request.ImportedCapabilities = test.projections
		if err := request.Validate(time.Now()); !errors.Is(err, contracts.ErrInvalidCapabilityProjection) {
			t.Fatalf("%s: error = %v, want ErrInvalidCapabilityProjection", test.name, err)
		}
	}
	request = validContext()
	request.ImportedCapabilities = make([]contracts.CapabilityProjection, 65)
	for i := range request.ImportedCapabilities {
		request.ImportedCapabilities[i] = contracts.CapabilityProjection{ID: fmt.Sprintf("provider.cap.%d", i), Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}
	}
	if err := request.Validate(time.Now()); !errors.Is(err, contracts.ErrInvalidCapabilityProjection) {
		t.Fatalf("oversized projections error = %v, want ErrInvalidCapabilityProjection", err)
	}
}

func TestChildPreservesCapabilityProjections(t *testing.T) {
	t.Parallel()
	request := validContext()
	request.ImportedCapabilities = []contracts.CapabilityProjection{{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}}
	child := request.Child()
	if child.CallDepth != request.CallDepth+1 || len(child.ImportedCapabilities) != 1 ||
		child.ImportedCapabilities[0].ID != "provider.cap" {
		t.Fatalf("child context=%#v", child)
	}
	if err := child.Validate(time.Now()); err != nil {
		t.Fatalf("child context rejected: %v", err)
	}
}
