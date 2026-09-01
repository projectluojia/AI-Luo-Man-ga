package contracts_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

func TestRequestContextValidatesCapabilityProjection(t *testing.T) {
	valid := contracts.CapabilityProjection{
		ID: "provider.capability", Version: "1.0.0",
		InputSchemaJSON: `{"type":"object"}`, RequiredPermissions: []string{"bus.read"},
	}
	tests := []struct {
		name       string
		projection contracts.CapabilityProjection
	}{
		{name: "invalid id", projection: contracts.CapabilityProjection{ID: "Provider.Capability", Version: "1.0.0", InputSchemaJSON: `{}`}},
		{name: "invalid version", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1", InputSchemaJSON: `{}`}},
		{name: "invalid schema", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{"type":`}},
		{name: "invalid permission", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{}`, RequiredPermissions: []string{"Bus.Read"}}},
		{name: "duplicate permission", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{}`, RequiredPermissions: []string{"bus.read", "bus.read"}}},
		{name: "oversized schema", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{"value":"` + strings.Repeat("x", contracts.MaxProjectionSchemaBytes) + `"}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			request.ImportedCapabilities = []contracts.CapabilityProjection{test.projection}
			if err := request.Validate(time.Now()); !errors.Is(err, contracts.ErrInvalidCapabilityProjection) {
				t.Fatalf("Validate() = %v, want ErrInvalidCapabilityProjection", err)
			}
		})
	}

	request := validRequest()
	request.ImportedCapabilities = make([]contracts.CapabilityProjection, contracts.MaxCapabilityProjections+1)
	for index := range request.ImportedCapabilities {
		request.ImportedCapabilities[index] = contracts.CapabilityProjection{
			ID: fmt.Sprintf("provider.capability.%d", index), Version: "1.0.0", InputSchemaJSON: `{}`,
		}
	}
	if err := request.Validate(time.Now()); !errors.Is(err, contracts.ErrInvalidCapabilityProjection) {
		t.Fatalf("oversized projection list error = %v, want ErrInvalidCapabilityProjection", err)
	}

	request = validRequest()
	request.ImportedCapabilities = []contracts.CapabilityProjection{valid}
	if err := request.Validate(time.Now()); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}

	request = validRequest()
	for index := 0; index < 5; index++ {
		request.ImportedCapabilities = append(request.ImportedCapabilities, contracts.CapabilityProjection{
			ID: fmt.Sprintf("provider.capability.%d", index), Version: "1.0.0",
			InputSchemaJSON: `{"value":"` + strings.Repeat("x", 60_000) + `"}`,
		})
	}
	if err := request.Validate(time.Now()); !errors.Is(err, contracts.ErrInvalidCapabilityProjection) {
		t.Fatalf("aggregate projection size error = %v, want ErrInvalidCapabilityProjection", err)
	}
}

func validRequest() contracts.RequestContext {
	return contracts.RequestContext{AppID: "app", EchoID: "echo", RequestID: "request", Deadline: time.Now().Add(time.Minute)}
}
