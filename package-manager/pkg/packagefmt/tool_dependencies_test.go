package packagefmt

import (
	"path/filepath"
	"testing"
)

func TestParsePreservesExplicitEmptyToolDependencies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.pkg.wasm"

[tool."demo.text"]
description = "字符串长度"
schema = "{}"
side_effect = "read"

[service]
tool_dependencies = []
`)
	manifest, _, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	extensions := decodeExtensions(t, manifest.Extensions)
	if len(extensions.Service.ToolDependencies) != 0 {
		t.Fatalf("显式空 tool_dependencies 未保留: %+v", extensions.Service.ToolDependencies)
	}
}
