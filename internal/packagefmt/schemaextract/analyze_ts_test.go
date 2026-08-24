package schemaextract

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTSFunctions(t *testing.T) {
	source := []byte(`export function hello(args: HelloArgs): any {}
export async function ping(args: PingArgs): Promise<any> {}
function hidden() {} // 非 export 忽略
export function noArgs() {}
`)
	names := extractTSFunctions(source)
	if len(names) != 2 || names[0] != "hello" || names[1] != "ping" {
		t.Fatalf("names = %v", names)
	}
}

func TestAnalyzeTSWithFakeRunner(t *testing.T) {
	source := []byte(`// hello 说你好。
export function hello(args: HelloArgs): any {
  return { message: "hello" };
}
`)
	dir := t.TempDir()
	fakeRunner := func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
		return json.Marshal(map[string]any{
			"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		})
	}
	capabilities, err := analyzeTS(context.Background(), source, "my.pkg", dir, fakeRunner)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != "my.pkg.hello" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if capabilities[0].Description != "hello 说你好。" {
		t.Fatalf("description = %q", capabilities[0].Description)
	}
	var schema map[string]any
	json.Unmarshal(capabilities[0].InputSchema, &schema)
	if schema["additionalProperties"] != false {
		t.Fatalf("schema 缺少 additionalProperties:false: %+v", schema)
	}
}

func TestAnalyzeTSWithRealNode(t *testing.T) {
	if _, err := execCommand(context.Background(), ".", "node", "--version"); err != nil {
		t.Skip("node 不可用，跳过真实 TS 集成测试")
	}
	source := []byte(`// hello 说你好。
export function hello(args: HelloArgs): any {
  return { message: "hello, " + args.name };
}
export interface HelloArgs {
  name: string;
  count?: number;
}
`)
	dir := t.TempDir()
	// 写临时 package.json 使 npx 能解析 ts-json-schema-generator
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","private":true}`), 0644)
	capabilities, err := AnalyzeTS(context.Background(), source, "my.pkg", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != "my.pkg.hello" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	t.Logf("生成的 schema: %s", capabilities[0].InputSchema)
	var schema map[string]any
	json.Unmarshal(capabilities[0].InputSchema, &schema)
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
}
