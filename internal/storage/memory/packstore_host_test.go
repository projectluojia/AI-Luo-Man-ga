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
	payload := []byte(`{"scope":"user","collection":"routes","id":"route-a","doc":{"name":"A"}}`)
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.capability", IdempotencyKey: "call-1",
	}, payload); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("read Capability write error=%v, want ErrAccessDenied", err)
	}
	// 治理上下文没有 UserID 时，声明 user 作用域也写不进去。
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.write", IdempotencyKey: "call-1",
	}, payload); !errors.Is(err, packstore.ErrInvalidScope) {
		t.Fatalf("anonymous user-scope write error=%v, want ErrInvalidScope", err)
	}
	// 系统作用域写入没有 guest 路径。
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.write", UserID: "user-a", IdempotencyKey: "call-1",
	}, []byte(`{"collection":"routes","id":"route-a","doc":{"name":"A"}}`)); !errors.Is(err, packstore.ErrAccessDenied) {
		t.Fatalf("system-scope write error=%v, want ErrAccessDenied（guest 写只落个人作用域）", err)
	}
	if _, err := putFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.write", UserID: "user-a", IdempotencyKey: "call-1",
	}, payload); err != nil {
		t.Fatalf("user-scope write Capability error=%v", err)
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

// TestHostUserScopeIsolationBetweenUsers 验证个人作用域按治理上下文注入的
// UserID 物理隔离：guest 只能声明作用域种类，不同用户互不可见，个人数据
// 与系统快照数据不混合，个人读取不携带快照元数据。
func TestHostUserScopeIsolationBetweenUsers(t *testing.T) {
	docs := memory.NewDocuments()
	// 播种系统快照数据（可信 Go 侧路径）。
	scope := packstore.Scope{AppID: "app-a", PackageID: "test", Namespace: "test/pkg"}
	importedAt := time.Now().UTC().Add(-time.Hour)
	meta := packstore.SnapshotMeta{
		Revision: "rev-1", Source: "test-source", Authoritative: true,
		Complete: true, ImportedAt: importedAt, ValidUntil: importedAt.Add(time.Hour),
	}
	if err := docs.ReplaceSnapshot(context.Background(), scope, meta, map[string][]packstore.Document{
		"routes": {{ID: "route-system", Payload: []byte(`{"id":"route-system"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	write := capability.CapabilitySpec{
		ID: "test.write", Version: "1.0.0", Name: "写入能力",
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectState, Replay: capability.ReplayIdempotencyKey, ConfirmationFloor: capability.ConfirmationPolicy},
	}
	functions := packstore.HostFunctions(docs, "test", "test/pkg", append(testCapabilities(), write))
	putFn := hostFunctionByName(functions, packstore.OpPut)
	getFn := hostFunctionByName(functions, packstore.OpGet)
	listFn := hostFunctionByName(functions, packstore.OpList)

	userPayload := func(user, id string) []byte {
		return []byte(`{"scope":"user","collection":"routes","id":"` + id + `","doc":{"id":"` + id + `"}}`)
	}
	for _, user := range []string{"user-a", "user-b"} {
		if _, err := putFn.Call(context.Background(), contracts.RequestContext{
			AppID: "app-a", CapabilityID: "test.write", UserID: user, IdempotencyKey: "call-" + user,
		}, userPayload(user, "route-"+user)); err != nil {
			t.Fatalf("write as %s: %v", user, err)
		}
	}
	// user-a 只能读到自己的文档与系统数据，看不到 user-b 的文档。
	read, err := getFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", UserID: "user-a", CapabilityID: "test.capability",
	}, []byte(`{"scope":"user","collection":"routes","id":"route-user-b"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(read, &decoded); err != nil || decoded.Found {
		t.Fatalf("user-a read user-b doc: found=%v err=%v", decoded.Found, err)
	}
	if _, err := getFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", UserID: "user-a", CapabilityID: "test.capability",
	}, []byte(`{"scope":"user","collection":"routes","id":"route-user-a"}`)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read, &decoded); err != nil {
		t.Fatal(err)
	}
	// 个人 list 不含系统文档，也不携带快照元数据。
	listed, err := listFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", UserID: "user-a", CapabilityID: "test.capability",
	}, []byte(`{"scope":"user","collection":"routes","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	var listDecoded struct {
		Docs      []struct{ ID string } `json:"docs"`
		MetaFound bool                  `json:"meta_found"`
	}
	if err := json.Unmarshal(listed, &listDecoded); err != nil {
		t.Fatal(err)
	}
	if len(listDecoded.Docs) != 1 || listDecoded.Docs[0].ID != "route-user-a" || listDecoded.MetaFound {
		t.Fatalf("user list docs=%#v metaFound=%v", listDecoded.Docs, listDecoded.MetaFound)
	}
	// 系统读取只含快照文档，不含任何个人文档。
	systemListed, err := listFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", CapabilityID: "test.capability",
	}, []byte(`{"collection":"routes","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	var systemDecoded struct {
		Docs      []struct{ ID string } `json:"docs"`
		MetaFound bool                  `json:"meta_found"`
	}
	if err := json.Unmarshal(systemListed, &systemDecoded); err != nil {
		t.Fatal(err)
	}
	if len(systemDecoded.Docs) != 1 || systemDecoded.Docs[0].ID != "route-system" || !systemDecoded.MetaFound {
		t.Fatalf("system list docs=%#v metaFound=%v", systemDecoded.Docs, systemDecoded.MetaFound)
	}
	// 未知作用域种类 fail-closed。
	if _, err := getFn.Call(context.Background(), contracts.RequestContext{
		AppID: "app-a", UserID: "user-a", CapabilityID: "test.capability",
	}, []byte(`{"scope":"global","collection":"routes","id":"x"}`)); !errors.Is(err, packstore.ErrInvalidScope) {
		t.Fatalf("unknown scope kind error=%v, want ErrInvalidScope", err)
	}
}
