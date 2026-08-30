package loader

import (
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestCapabilityProjectionEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	projections := []contracts.CapabilityProjection{
		{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`, RequiredPermissions: []string{"bus.admin", "bus.read"}},
		{ID: "other.cap", Version: "2.0.0", InputSchemaJSON: `{"type":"string"}`},
	}
	decoded, err := decodeCapabilityProjections(encodeCapabilityProjections(projections))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, projections) {
		t.Fatalf("round trip lost fidelity: %#v", decoded)
	}
	// nil 投影项属于协议违例，解码边界必须直接拒绝。
	if _, err := decodeCapabilityProjections([]*runtimev1.CapabilityProjection{nil}); err == nil {
		t.Fatal("nil projection entry accepted")
	}
}

func TestEncodeCapabilityProjectionsCopiesCallerOwnedSlices(t *testing.T) {
	t.Parallel()
	permissions := []string{"bus.read"}
	source := []contracts.CapabilityProjection{{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`, RequiredPermissions: permissions}}
	encoded := encodeCapabilityProjections(source)
	permissions[0] = "mutated"
	if len(encoded) != 1 || len(encoded[0].RequiredPermissions) != 1 || encoded[0].RequiredPermissions[0] != "bus.read" {
		t.Fatalf("encode aliased caller-owned permissions: %#v", encoded)
	}
}

func validProjectionInvokeRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app", EchoID: "echo", RequestID: "request", ProtocolVersion: "1.0",
		TargetType:      registry.TargetTypeCapability,
		CapabilityID:    "campus.bus.routes.list",
		ServiceID:       "campus",
		CallDepth:       1,
		PermissionScope: []string{"bus.read"},
	}
}

func TestValidateRuntimeInvokeRejectsMalformedCapabilityProjections(t *testing.T) {
	t.Parallel()
	valid := contracts.CapabilityProjection{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}
	tests := []struct {
		name       string
		projection contracts.CapabilityProjection
	}{
		{name: "缺失 ID", projection: contracts.CapabilityProjection{Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}},
		{name: "缺失版本", projection: contracts.CapabilityProjection{ID: "provider.cap", InputSchemaJSON: `{"type":"object"}`}},
		{name: "Schema 非法 JSON", projection: contracts.CapabilityProjection{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"type":`}},
		{name: "ID 携带控制字符", projection: contracts.CapabilityProjection{ID: "provider.cap\n", Version: "1.0.0", InputSchemaJSON: `{"type":"object"}`}},
		{name: "版本非法 UTF-8", projection: contracts.CapabilityProjection{ID: "provider.cap", Version: "1.\xff0", InputSchemaJSON: `{"type":"object"}`}},
		{name: "Schema 超长", projection: contracts.CapabilityProjection{ID: "provider.cap", Version: "1.0.0", InputSchemaJSON: `{"pad":"` + strings.Repeat("x", maxContextValueBytes) + `"}`}},
	}
	for _, test := range tests {
		request := validProjectionInvokeRequest()
		request.ImportedCapabilities = []contracts.CapabilityProjection{test.projection}
		if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err == nil {
			t.Fatalf("%s: malformed projection accepted", test.name)
		}
	}
	// 超过数量上限的投影必须整体拒绝。
	request := validProjectionInvokeRequest()
	request.ImportedCapabilities = make([]contracts.CapabilityProjection, 0, maxContextItems+1)
	for i := 0; i <= maxContextItems; i++ {
		request.ImportedCapabilities = append(request.ImportedCapabilities, valid)
	}
	if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err == nil {
		t.Fatal("oversized projection list accepted")
	}
	// 合法投影必须通过。
	request = validProjectionInvokeRequest()
	request.ImportedCapabilities = []contracts.CapabilityProjection{valid}
	if err := validateRuntimeInvoke(request, []byte(`{}`), time.Now()); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
}
