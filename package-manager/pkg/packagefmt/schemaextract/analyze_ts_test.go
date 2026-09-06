package schemaextract

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTSFunctions(t *testing.T) {
	source := []byte(`export function hello(args: HelloArgs): any {}
export async function ping(args: PingArgs): Promise<any> {}
function hidden() {} // 非 export 忽略
export function noArgs() {}
export function untyped(args): any {}
export function wrongType(args: OtherArgs): any {}
export function tooMany(args: HelloArgs, other: string): any {}
`)
	names := extractTSFunctions(source)
	if len(names) != 2 || names[0] != "hello" || names[1] != "ping" {
		t.Fatalf("names = %v", names)
	}
}

func TestExtractTSFunctionsRequiresContractParameter(t *testing.T) {
	source := []byte(`export function hello(args: HelloArgs): any {}
export function hello2(args: Hello2Args): any {}
export function noArgs(): any {}
export function wrong(args: Wrong): any {}
`)
	names := extractTSFunctions(source)
	if len(names) != 2 || names[0] != "hello" || names[1] != "hello2" {
		t.Fatalf("names = %v, want only functions with matching Args parameter", names)
	}
}

func TestAnalyzeTSWithFakeRunner(t *testing.T) {
	source := []byte(`// hello 说你好。
export function hello(args: HelloArgs): any {
  return { message: "hello" };
}
`)
	dir := t.TempDir()
	fakeRunner := func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		wantArgs := "--yes --package " + SchemaGeneratorPackage + " ts-json-schema-generator --path main.ts --type HelloArgs --no-top-ref"
		if name != "npx" || strings.Join(args, " ") != wantArgs {
			t.Fatalf("schema generator command = %s %s", name, strings.Join(args, " "))
		}
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
	if err := json.Unmarshal(capabilities[0].InputSchema, &schema); err != nil {
		t.Fatalf("schema 解码失败: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema 缺少 additionalProperties:false: %+v", schema)
	}
}

func TestAnalyzeTSWithRealNode(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx 不可用，跳过真实 TS 集成测试")
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
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	capabilities, err := AnalyzeTS(context.Background(), source, "my.pkg", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != "my.pkg.hello" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	t.Logf("生成的 schema: %s", capabilities[0].InputSchema)
	var schema map[string]any
	if err := json.Unmarshal(capabilities[0].InputSchema, &schema); err != nil {
		t.Fatalf("schema 解码失败: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
}
