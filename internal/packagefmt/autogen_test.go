package packagefmt

import (
	"encoding/json"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packagefmt/schemaextract"
)

// TestManifestFromCapabilities 验证从源码提取的 capabilities 自动生成完整清单。
func TestManifestFromCapabilities(t *testing.T) {
	source := []byte(`package main

// hello 说你好。
func hello(args HelloArgs) {}

type HelloArgs struct {
	Name string ` + "`json:\"name\"`" + `
}
`)
	capabilities, err := schemaextract.AnalyzeGo(source, "hello.pkg")
	if err != nil {
		t.Fatal(err)
	}
	manifest, manifestBytes, err := ManifestFromCapabilities("hello.pkg", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "hello.pkg" || manifest.Version != "0.1.0" {
		t.Fatalf("manifest id/version = %q/%q", manifest.ID, manifest.Version)
	}
	if len(manifest.Components) != 1 || manifest.Components[0].Entrypoint != "main.wasm" {
		t.Fatalf("components = %+v", manifest.Components)
	}
	if len(manifest.Components[0].Exports) != 1 || manifest.Components[0].Exports[0] != "hello.pkg.hello" {
		t.Fatalf("exports = %v", manifest.Components[0].Exports)
	}
	// Extensions 必须包含 tool + capability，schema 与提取一致。
	var extensions struct {
		Tools []struct {
			ID              string `json:"id"`
			InputSchemaJSON string `json:"input_schema_json"`
		} `json:"tools"`
		Capabilities []struct {
			ID              string `json:"id"`
			InputSchemaJSON string `json:"input_schema_json"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(manifest.Extensions, &extensions); err != nil {
		t.Fatalf("extensions 解码失败: %v", err)
	}
	if len(extensions.Tools) != 1 || extensions.Tools[0].ID != "hello.pkg.hello" {
		t.Fatalf("tools = %+v", extensions.Tools)
	}
	if extensions.Tools[0].InputSchemaJSON != string(capabilities[0].InputSchema) {
		t.Fatalf("schema 不一致")
	}
	if len(manifestBytes) == 0 {
		t.Fatal("manifestBytes 为空")
	}
}

func TestManifestFromCapabilitiesRejectsEmpty(t *testing.T) {
	if _, _, err := ManifestFromCapabilities("hello.pkg", nil); err == nil {
		t.Fatal("期望拒绝空 capabilities")
	}
}
