package packagefmt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

// 完整源清单：验证继承规则（capability 继承 tool、service 自动生成）。
func TestParseInheritsToolAndService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "strings.tool"
version = "1.0.0"
description = "字符串工具"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "strings.tool.wasm"
exports = ["strings.len.cap"]

[tool."strings.len"]
description = "字符串长度"
schema = """{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}"""
side_effect = "read"

[[capability]]
id = "strings.len.cap"
tool = "strings.len"
`)

	manifest, _, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if manifest.ID != "strings.tool" || manifest.Version != "1.0.0" {
		t.Fatalf("包标识错误: %+v", manifest)
	}
	if len(manifest.Components) != 1 || manifest.Components[0].Entrypoint != "strings.tool.wasm" {
		t.Fatalf("组件错误: %+v", manifest.Components)
	}
	extensions := decodeExtensions(t, manifest.Extensions)
	if len(extensions.Tools) != 1 {
		t.Fatalf("tools 数量错误: %d", len(extensions.Tools))
	}
	tool := extensions.Tools[0]
	if tool.ID != "strings.len" || tool.Version != "1.0.0" || tool.SideEffect != "read" {
		t.Fatalf("tool 继承错误: %+v", tool)
	}
	// service 自动生成：id/version/description 继承 package，依赖默认全部 tool。
	if extensions.Service.ID != "strings.tool" || extensions.Service.Version != "1.0.0" {
		t.Fatalf("service 继承错误: %+v", extensions.Service)
	}
	if len(extensions.Service.ToolDependencies) != 1 || extensions.Service.ToolDependencies[0] != "strings.len" {
		t.Fatalf("service 依赖默认错误: %+v", extensions.Service.ToolDependencies)
	}
	if len(extensions.Capabilities) != 1 {
		t.Fatalf("capabilities 数量错误: %d", len(extensions.Capabilities))
	}
	capability := extensions.Capabilities[0]
	if capability.ID != "strings.len.cap" || capability.ToolID != "strings.len" {
		t.Fatalf("capability 标识错误: %+v", capability)
	}
	if capability.Version != "1.0.0" || capability.ServiceID != "strings.tool" {
		t.Fatalf("capability 继承版本/service 错误: %+v", capability)
	}
	// capability 继承 tool 的 schema、side_effect、name（= tool description）。
	if capability.InputSchemaJSON != tool.InputSchemaJSON {
		t.Fatalf("capability 未继承 tool schema")
	}
	if capability.SideEffect != "read" {
		t.Fatalf("capability 未继承 tool side_effect")
	}
	if capability.Name != "字符串长度" {
		t.Fatalf("capability name 未继承 tool description: %q", capability.Name)
	}
	if capability.Description != "字符串长度" {
		t.Fatalf("capability description 未继承 tool description: %q", capability.Description)
	}
}

// capability 显式覆盖 name/description 时优先于继承。
func TestParseCapabilityOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "2.1.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"
exports = ["demo.cap"]

[tool."demo"]
description = "演示"
schema = """{}"""
side_effect = "read"

[[capability]]
id = "demo.cap"
tool = "demo"
name = "演示能力"
description = "覆盖描述"
`)

	manifest, _, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	extensions := decodeExtensions(t, manifest.Extensions)
	if len(extensions.Capabilities) != 1 {
		t.Fatalf("capabilities 数量错误: %d", len(extensions.Capabilities))
	}
	capability := extensions.Capabilities[0]
	if capability.Name != "演示能力" || capability.Description != "覆盖描述" {
		t.Fatalf("capability 覆盖未生效: %+v", capability)
	}
}

// capability 引用不存在的 tool 必须拒绝。
func TestParseCapabilityUnknownTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"
exports = ["demo.cap"]

[[capability]]
id = "demo.cap"
tool = "missing.tool"
`)

	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("引用不存在的 tool 应报错")
	}
}

// 未知字段必须拒绝（严格解码，防止作者拼写错误静默忽略）。
func TestParseRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"
unknown_field = true

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"
`)

	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

// 没有 component 的源清单必须拒绝。
func TestParseRequiresComponent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"
`)

	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("缺少 component 应被拒绝")
	}
}

func writeSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// testExtensions 是测试断言用的扩展结构（与内核 registry 规格字段一致）。
type testExtensions struct {
	Tools        []testTool       `json:"tools"`
	Service      testService      `json:"service"`
	Capabilities []testCapability `json:"capabilities"`
}

type testTool struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	Description     string `json:"description"`
	InputSchemaJSON string `json:"input_schema_json"`
	SideEffect      string `json:"side_effect"`
}

type testService struct {
	ID               string   `json:"id"`
	Version          string   `json:"version"`
	Description      string   `json:"description"`
	ToolDependencies []string `json:"tool_dependencies"`
}

type testCapability struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	ServiceID       string `json:"service_id"`
	InputSchemaJSON string `json:"input_schema_json"`
	SideEffect      string `json:"side_effect"`
	ToolID          string `json:"tool_id"`
}

// decodeExtensions 解码 manifest.Extensions 为可断言的扩展结构。
func decodeExtensions(t *testing.T, raw json.RawMessage) testExtensions {
	t.Helper()
	var extensions testExtensions
	if len(raw) == 0 {
		t.Fatal("extensions 为空")
	}
	if err := json.Unmarshal(raw, &extensions); err != nil {
		t.Fatalf("解码 extensions: %v", err)
	}
	return extensions
}

// 端到端：ailuo.toml → PackFromSource → Install，验证作者侧源清单可完整打包
// 并安装（与 CLI `ailuo pack` 走同一路径）。
func TestPackAndInstallFromSourceManifest(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	path := filepath.Join(sourceDir, SourceFileName)
	writeSource(t, path, `
[package]
id = "strings.tool"
version = "1.0.0"
description = "字符串工具"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "strings.tool.wasm"
exports = ["strings.len.cap"]

[tool."strings.len"]
description = "字符串长度"
schema = """{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}"""
side_effect = "read"

[[capability]]
id = "strings.len.cap"
tool = "strings.len"
`)
	// 工件必须存在（pack 校验 entrypoint 为常规文件）。
	artifact := []byte("wasm-bytes")
	if err := os.WriteFile(filepath.Join(sourceDir, "strings.tool.wasm"), artifact, 0o640); err != nil {
		t.Fatalf("写工件: %v", err)
	}

	manifest, manifestBytes, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tarballPath, err := packmgr.PackFromSource(ctx, sourceDir, t.TempDir(), manifest, manifestBytes)
	if err != nil {
		t.Fatalf("PackFromSource: %v", err)
	}
	if filepath.Base(tarballPath) != "strings.tool-1.0.0.tgz" {
		t.Fatalf("tarball name = %s", filepath.Base(tarballPath))
	}
	record, err := packmgr.Install(ctx, t.TempDir(), tarballPath)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if record.Manifest.ID != "strings.tool" || record.Manifest.Version != "1.0.0" {
		t.Fatalf("安装记录错误: %+v", record.Manifest)
	}
	extensions := decodeExtensions(t, record.Manifest.Extensions)
	if len(extensions.Tools) != 1 || extensions.Tools[0].ID != "strings.len" {
		t.Fatalf("安装后 tools 错误: %+v", extensions.Tools)
	}
	if len(extensions.Capabilities) != 1 || extensions.Capabilities[0].ToolID != "strings.len" {
		t.Fatalf("安装后 capabilities 错误: %+v", extensions.Capabilities)
	}
}
