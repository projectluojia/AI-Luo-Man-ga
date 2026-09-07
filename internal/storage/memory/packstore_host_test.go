package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
)

// hostFunctionByName 取指定名称的存储宿主函数。
func hostFunctionByName(functions []loader.HostedFunction, name string) loader.HostedFunction {
	for _, fn := range functions {
		if fn.Name == name {
			return fn
		}
	}
	return loader.HostedFunction{}
}

func testCapabilities() []capability.CapabilitySpec {
	return []capability.CapabilitySpec{{
		ID: "test.capability", Version: "1.0.0", Name: "测试能力",
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
	}}
}

// TestHostListCallReturnsEnvelopeAndMeta 验证 ailuo.store list 宿主函数的
// 端到端信封：治理上下文注入 App、响应内嵌一致快照元数据。
func TestHostListCallReturnsEnvelopeAndMeta(t *testing.T) {
	docs := memory.NewDocuments()
	scope := packstore.Scope{AppID: "app-a", PackageID: "test", Namespace: "test/pkg"}
	importedAt := time.Now().UTC().Add(-time.Hour)
	meta := packstore.SnapshotMeta{
		Revision: "rev-1", Source: "test-source", Authoritative: true,
		Complete: true, ImportedAt: importedAt, ValidUntil: importedAt.Add(time.Hour),
	}
	if err := docs.ReplaceSnapshot(context.Background(), scope, meta, map[string][]packstore.Document{
		"routes": {{ID: "route-a", Payload: []byte(`{"id":"route-a","name":"A"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	listFn := hostFunctionByName(packstore.HostFunctions(docs, "test", "test/pkg", testCapabilities()), packstore.OpList)
	response, err := listFn.Call(context.Background(), contracts.RequestContext{AppID: "app-a", CapabilityID: "test.capability"}, []byte(`{"collection":"routes","limit":10}`))
	if err != nil {
		t.Fatalf("list call error: %v", err)
	}
	var decoded struct {
		Docs []struct {
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"doc"`
		} `json:"docs"`
		Meta struct {
			SourceRevision string `json:"source_revision"`
		} `json:"meta"`
		MetaFound bool `json:"meta_found"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("decode response %s: %v", response, err)
	}
	if len(decoded.Docs) != 1 || decoded.Docs[0].ID != "route-a" {
		t.Fatalf("docs=%#v", decoded.Docs)
	}
	if !decoded.MetaFound || decoded.Meta.SourceRevision != "rev-1" {
		t.Fatalf("meta=%#v found=%v", decoded.Meta, decoded.MetaFound)
	}
}

// TestHostGetCallReturnsEnvelope 验证 get 宿主函数的信封形状。
func TestHostGetCallReturnsEnvelope(t *testing.T) {
	docs := memory.NewDocuments()
	scope := packstore.Scope{AppID: "app-a", PackageID: "test", Namespace: "test/pkg"}
	if err := docs.Put(context.Background(), scope, "routes", "route-a", []byte(`{"id":"route-a"}`)); err != nil {
		t.Fatal(err)
	}
	getFn := hostFunctionByName(packstore.HostFunctions(docs, "test", "test/pkg", testCapabilities()), packstore.OpGet)
	response, err := getFn.Call(context.Background(), contracts.RequestContext{AppID: "app-a", CapabilityID: "test.capability"}, []byte(`{"collection":"routes","id":"route-a"}`))
	if err != nil {
		t.Fatalf("get call error: %v", err)
	}
	var decoded struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil || !decoded.Found {
		t.Fatalf("response=%s found=%v err=%v", response, decoded.Found, err)
	}
}

// TestHostCallRejectsForeignScope 验证治理上下文缺失 AppID 时 fail-closed。
func TestHostCallRejectsForeignScope(t *testing.T) {
	docs := memory.NewDocuments()
	listFn := hostFunctionByName(packstore.HostFunctions(docs, "test", "test/pkg", testCapabilities()), packstore.OpList)
	if _, err := listFn.Call(context.Background(), contracts.RequestContext{}, []byte(`{"collection":"routes","limit":10}`)); err == nil {
		t.Fatal("empty AppID call unexpectedly succeeded")
	}
}

func TestHostWriteRequiresWriteCapabilityAndIdempotency(t *testing.T) {
	docs := memory.NewDocuments()
	read := testCapabilities()
	write := capability.CapabilitySpec{
		ID: "test.write", Version: "1.0.0", Name: "写入能力",
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectState, Replay: capability.ReplayIdempotencyKey, ConfirmationFloor: capability.ConfirmationPolicy},
	}
	functions := packstore.HostFunctions(docs, "test", "test/pkg", append(read, write))
	putFn := hostFunctionByName(functions, packstore.OpPut)
	payload := []byte(`{"collection":"routes","id":"route-a","doc":{"name":"A"}}`)
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.capability", IdempotencyKey: "call-1",
	}, payload); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("read Capability write error=%v, want ErrAccessDenied", err)
	}
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.write",
	}, payload); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("write without idempotency error=%v, want ErrAccessDenied", err)
	}
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.write", IdempotencyKey: "call-1",
	}, payload); err != nil {
		t.Fatalf("write Capability error=%v", err)
	}
}

// TestHostEffectNoneCapabilityCannotWrite 验证 EffectNone Capability 即使声明
// 幂等键 Replay 也无法执行写操作（无副作用承诺不被幂等/确认绕过）。
func TestHostEffectNoneCapabilityCannotWrite(t *testing.T) {
	docs := memory.NewDocuments()
	sideEffectFree := capability.CapabilitySpec{
		ID: "test.none.idempotent", Version: "1.0.0", Name: "无副作用但幂等键",
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplayIdempotencyKey, ConfirmationFloor: capability.ConfirmationPolicy},
	}
	functions := packstore.HostFunctions(docs, "test", "test/pkg", append(testCapabilities(), sideEffectFree))
	putFn := hostFunctionByName(functions, packstore.OpPut)
	deleteFn := hostFunctionByName(functions, packstore.OpDelete)
	payload := []byte(`{"collection":"routes","id":"route-a","doc":{"name":"A"}}`)
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.none.idempotent", IdempotencyKey: "call-1",
	}, payload); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("EffectNone put error=%v, want ErrAccessDenied", err)
	}
	if _, err := deleteFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.none.idempotent", IdempotencyKey: "call-2",
	}, []byte(`{"collection":"routes","id":"route-a"}`)); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("EffectNone delete error=%v, want ErrAccessDenied", err)
	}
}

func TestHostRejectsNamespaceOwnedByAnotherPackage(t *testing.T) {
	docs := memory.NewDocuments()
	listFn := hostFunctionByName(packstore.HostFunctions(docs, "test", "other/pkg", testCapabilities()), packstore.OpList)
	_, err := listFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.capability",
	}, []byte(`{"collection":"routes","limit":10}`))
	if !errors.Is(err, packstore.ErrInvalidScope) {
		t.Fatalf("foreign namespace error=%v, want ErrInvalidScope", err)
	}
}

func TestHostCallRejectsNonCanonicalRequestJSON(t *testing.T) {
	docs := memory.NewDocuments()
	listFn := hostFunctionByName(packstore.HostFunctions(docs, "test", "test/pkg", testCapabilities()), packstore.OpList)
	for _, payload := range []string{
		`{"collection":"routes","limit":10,"extra":true}`,
		`{"collection":"routes","limit":10,"limit":10}`,
		`{"collection":"routes","limit":10} trailing`,
	} {
		_, err := listFn.Call(context.Background(), contracts.RequestContext{AppID: "app-a", CapabilityID: "test.capability"}, []byte(payload))
		if !errors.Is(err, packstore.ErrInvalidKey) {
			t.Errorf("payload %q error=%v, want strict request rejection", payload, err)
		}
	}
}
