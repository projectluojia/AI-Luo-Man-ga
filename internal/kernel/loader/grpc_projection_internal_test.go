package loader

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestCapabilityProjectionEncodeDecodeRoundTrip(t *testing.T) {
	projections := []contracts.CapabilityProjection{
		{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`, RequiredPermissions: []string{"bus.read"}},
		{ID: "other.capability", Version: "2.0.0", InputSchemaJSON: `{"type":"string"}`},
	}
	decoded, err := decodeCapabilityProjections(encodeCapabilityProjections(projections))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, projections) {
		t.Fatalf("round trip lost fidelity: %#v", decoded)
	}
	if _, err := decodeCapabilityProjections([]*runtimev1.CapabilityProjection{nil}); err == nil {
		t.Fatal("nil projection entry accepted")
	}
}

func TestEncodeCapabilityProjectionsCopiesPermissions(t *testing.T) {
	permissions := []string{"bus.read"}
	source := []contracts.CapabilityProjection{{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{}`, RequiredPermissions: permissions}}
	encoded := encodeCapabilityProjections(source)
	permissions[0] = "mutated"
	if len(encoded) != 1 || len(encoded[0].RequiredPermissions) != 1 || encoded[0].RequiredPermissions[0] != "bus.read" {
		t.Fatalf("encoded projection aliases caller permissions: %#v", encoded)
	}
}

func TestValidateRuntimeInvokeRejectsMalformedCapabilityProjections(t *testing.T) {
	valid := contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{}`}
	for _, test := range []struct {
		name       string
		projection contracts.CapabilityProjection
	}{
		{name: "invalid id", projection: contracts.CapabilityProjection{ID: "Provider.Capability", Version: "1.0.0", InputSchemaJSON: `{}`}},
		{name: "invalid version", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1", InputSchemaJSON: `{}`}},
		{name: "invalid schema", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{"type":`}},
		{name: "invalid permission", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{}`, RequiredPermissions: []string{"Bus.Read"}}},
		{name: "oversized schema", projection: contracts.CapabilityProjection{ID: "provider.capability", Version: "1.0.0", InputSchemaJSON: `{"value":"` + strings.Repeat("x", contracts.MaxProjectionSchemaBytes) + `"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := projectionRequest()
			request.ImportedCapabilities = []contracts.CapabilityProjection{test.projection}
			if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err == nil {
				t.Fatal("malformed projection accepted")
			}
		})
	}

	request := projectionRequest()
	request.ImportedCapabilities = make([]contracts.CapabilityProjection, 0, contracts.MaxCapabilityProjections+1)
	for index := 0; index <= contracts.MaxCapabilityProjections; index++ {
		request.ImportedCapabilities = append(request.ImportedCapabilities, contracts.CapabilityProjection{
			ID: fmt.Sprintf("provider.capability.%d", index), Version: "1.0.0", InputSchemaJSON: `{}`,
		})
	}
	if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err == nil {
		t.Fatal("oversized projection list accepted")
	}

	request = projectionRequest()
	request.ImportedCapabilities = []contracts.CapabilityProjection{valid}
	if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
}

func projectionRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app", EchoID: "echo", RequestID: "request", ProtocolVersion: "3.0",
		TargetType: registry.TargetTypeCapability, CapabilityID: "target.capability", ServiceID: "target.service",
		PermissionScope: []string{"bus.read"},
	}
}
