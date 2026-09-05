package packagefmt

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt/schemaextract"
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
	capabilities, err := schemaextract.AnalyzeGo(source, "autogen.test")
	if err != nil {
		t.Fatal(err)
	}
	manifest, manifestBytes, err := ManifestFromCapabilities("autogen.test", "1.2.3", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "autogen.test" || manifest.Version != "1.2.3" {
		t.Fatalf("manifest id/version = %q/%q", manifest.ID, manifest.Version)
	}
	if len(manifest.Components) != 1 || manifest.Components[0].Entrypoint != "main.wasm" {
		t.Fatalf("components = %+v", manifest.Components)
	}
	if len(manifest.Components[0].Exports) != 1 || manifest.Components[0].Exports[0] != "autogen.test.hello" {
		t.Fatalf("exports = %v", manifest.Components[0].Exports)
	}
	if len(manifest.Capabilities) != 1 || manifest.Capabilities[0].ID != "autogen.test.hello" {
		t.Fatalf("capabilities = %+v", manifest.Capabilities)
	}
	if manifest.Capabilities[0].InputSchemaJSON != string(capabilities[0].InputSchema) {
		t.Fatalf("schema 不一致")
	}
	if len(manifestBytes) == 0 {
		t.Fatal("manifestBytes 为空")
	}
}

func TestManifestFromCapabilitiesRejectsEmpty(t *testing.T) {
	if _, _, err := ManifestFromCapabilities("autogen.test", "1.0.0", nil); err == nil {
		t.Fatal("期望拒绝空 capabilities")
	}
}
