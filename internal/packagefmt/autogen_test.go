package packagefmt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
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
	manifest, manifestBytes, err := ManifestFromCapabilities("hello.pkg", "1.2.3", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "hello.pkg" || manifest.Version != "1.2.3" {
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
	if _, _, err := ManifestFromCapabilities("hello.pkg", "1.0.0", nil); err == nil {
		t.Fatal("期望拒绝空 capabilities")
	}
}

func TestZeroDeclarationCapabilityRunsThroughDispatcherAndWasmHost(t *testing.T) {
	ctx := context.Background()
	sourceDir := filepath.Join(t.TempDir(), "hello.pkg")
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hello.pkg", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), source, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module hello-e2e\n\ngo 1.26\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	capabilities, buildTool, err := AutoExtract(ctx, sourceDir)
	if err != nil {
		t.Fatalf("AutoExtract: %v", err)
	}
	if buildTool != BuildToolGoWasm || len(capabilities) != 1 {
		t.Fatalf("extraction = %s %#v", buildTool, capabilities)
	}
	manifest, _, err := ManifestFromCapabilities("hello.pkg", "1.0.0", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, sourceDir, manifest, BuildSpec{Tool: buildTool}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	artifactPath := filepath.Join(sourceDir, "main.wasm")
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(context.Context, loader.Manifest) ([]byte, error) {
			return os.ReadFile(artifactPath)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := "hello.pkg.main"
	if err := manager.Register(ctx, loader.Manifest{
		ID: runtimeID, Version: manifest.Version, Mode: loader.ModeHosted,
		Role: loader.RoleCapability, LockedDigest: hex.EncodeToString(digest[:]),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	reg := registry.New()
	capability := capabilities[0]
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: manifest.ID, Version: manifest.Version},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			capability.ID: {
				Spec: registry.CapabilitySpec{
					ID: capability.ID, Version: manifest.Version, ServiceID: manifest.ID,
					InputSchemaJSON: string(capability.InputSchema), SideEffect: registry.SideEffectRead,
					ToolID: capability.ID,
				},
				Handler: manager.Handler(runtimeID),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(manifest.ID, capability.ID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	result, err := dispatcher.InvokeCapability(ctx, contracts.RequestContext{
		AppID: manifest.ID, EchoID: "echo-1", RequestID: "request-1",
		UserID: "user-1", Deadline: time.Now().Add(time.Minute),
	}, capability.ID, json.RawMessage(`{"name":"Ada"}`))
	if err != nil {
		t.Fatalf("InvokeCapability: %v", err)
	}
	if string(result) != `{"message":"hello, Ada"}` {
		t.Fatalf("result = %s", result)
	}
}
