package packagefmt

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
)

func TestParseUsesExplicitCapabilities(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"
description = "字符串能力"

[[component]]
id = "core"
mode = "hosted"
role = "provider"
entrypoint = "demo.pkg.wasm"
exports = ["demo.text.cap"]

[[capability]]
id = "demo.text.cap"
name = "字符串长度"
description = "计算字符串长度"
schema = """{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}"""
side_effect = "read"
required_permissions = ["demo.read"]
`)

	manifest, _, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if manifest.ID != "demo.pkg" || manifest.Version != "1.0.0" {
		t.Fatalf("包标识错误: %+v", manifest)
	}
	if len(manifest.Components) != 1 || manifest.Components[0].Role != packagecontract.RoleProvider {
		t.Fatalf("组件错误: %+v", manifest.Components)
	}
	if len(manifest.Capabilities) != 1 {
		t.Fatalf("capabilities 数量错误: %d", len(manifest.Capabilities))
	}
	spec := manifest.Capabilities[0]
	if spec.ID != "demo.text.cap" || spec.Version != "1.0.0" || spec.Name != "字符串长度" ||
		spec.Description != "计算字符串长度" || spec.SideEffect != "read" ||
		len(spec.RequiredPermissions) != 1 || spec.RequiredPermissions[0] != "demo.read" {
		t.Fatalf("capability 解析错误: %+v", spec)
	}
	if len(manifest.Components[0].Exports) != 1 || manifest.Components[0].Exports[0] != spec.ID {
		t.Fatalf("exports 错误: %v", manifest.Components[0].Exports)
	}
}

func TestParseIsolatedProcessTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "agent"
version = "1.0.0"

[[component]]
id = "executor"
mode = "isolated"
role = "executor"
entrypoint = "runtime"

[component.process]
path = "bin/runner"
args = ["--listen", "${address}"]
work_dir = "."
address = "127.0.0.1:50051"
`)

	manifest, _, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	component := manifest.Components[0]
	if component.Role != packagecontract.RoleExecutor || component.Process == nil {
		t.Fatalf("component = %+v, want executor template", component)
	}
	if component.Process.Path != "bin/runner" || component.Process.WorkDir != "." ||
		component.Process.Address != "127.0.0.1:50051" {
		t.Fatalf("process = %+v, want parsed template", component.Process)
	}
}

func TestParseRejectsLegacyToolAndServiceFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
role = "provider"
entrypoint = "demo.wasm"

[tool."demo.text"]
description = "旧模型"

[service]
id = "旧模型"
`)
	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("旧 Tool/Service 清单字段必须被严格拒绝")
	}
}

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
role = "provider"
entrypoint = "demo.wasm"
`)

	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

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

func TestPackAndInstallFromSourceManifest(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	path := filepath.Join(sourceDir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"
description = "字符串能力"

[[component]]
id = "core"
mode = "hosted"
role = "provider"
entrypoint = "demo.pkg.wasm"
exports = ["demo.text.cap"]

[[capability]]
id = "demo.text.cap"
name = "字符串长度"
description = "计算字符串长度"
schema = """{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}"""
side_effect = "read"
`)
	artifact := []byte("wasm-bytes")
	if err := os.WriteFile(filepath.Join(sourceDir, "demo.pkg.wasm"), artifact, 0o640); err != nil {
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
	if filepath.Base(tarballPath) != "demo.pkg-1.0.0.tgz" {
		t.Fatalf("tarball name = %s", filepath.Base(tarballPath))
	}
	installRoot := t.TempDir()
	record, err := packmgr.Install(ctx, installRoot, tarballPath)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if record.Manifest.ID != "demo.pkg" || record.Manifest.Version != "1.0.0" || len(record.Manifest.Capabilities) != 1 {
		t.Fatalf("安装记录错误: %+v", record.Manifest)
	}
	if record.Manifest.Capabilities[0].ID != "demo.text.cap" {
		t.Fatalf("安装后 capability 错误: %+v", record.Manifest.Capabilities)
	}
}

func writeSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
