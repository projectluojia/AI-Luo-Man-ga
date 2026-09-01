//go:build unix

package loader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// TestRuntimeHostProductionWiring 验证外部 Runtime Host 产品接线：真实安装目录
// （manifest.json + lock.json + 真实 wasm 工件）经 Catalog.ReadArtifact 供给
// hostedRuntimeBackend，完整 RuntimeHost 协议链路可装载并执行 hosted 包。
// 此前生产路径因 ReadArtifact 对 Role/LockedDigest/Pin/IdleTTL 的全字段比较
// 而必然失败，既有协议测试以 stub ReadArtifact 掩盖了该问题。
func TestRuntimeHostProductionWiring(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, testPackageID)
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifactBytes := hostedArtifact(t, filepath.Join("success", "success.wasm"))
	artifactPath := filepath.Join(directory, "success.wasm")
	if err := os.WriteFile(artifactPath, artifactBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	extensions, err := json.Marshal(map[string]any{
		"tools": []capability.ToolSpec{{
			ID: testToolID, Version: "1.0.0", Description: "测试回显",
			InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
		"service": capability.ServiceSpec{
			ID: testPackageID, Version: "1.0.0", Description: "通用运行时测试",
			ToolDependencies: []string{testToolID},
		},
		"capabilities": []capability.CapabilitySpec{{
			ID: testCapabilityID, Version: "1.0.0", Name: "测试回显",
			Description: "测试回显", ServiceID: testPackageID,
			InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: testPackageID, Version: "1.0.0",
		Pin: true, Extensions: extensions,
		Components: []packagecontract.Component{{
			ID: "runtime", Mode: loader.ModeHosted, Entrypoint: "success.wasm",
			Exports: []string{testCapabilityID},
		}},
	}
	manifest, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	artifactDigest := sha256.Sum256(artifactBytes)
	lock := packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: testPackageID,
		PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: "runtime", Path: artifactPath, SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := discoverCatalogLocked(t, catalog, root)
	if err != nil || len(records) != 1 || records[0].Runtime.ID != testRuntimeID {
		t.Fatalf("discover records=%#v err=%v", records, err)
	}
	backend, err := loader.NewHostedRuntimeBackend(loader.WasmHostConfig{ReadArtifact: catalog.ReadArtifact})
	if err != nil {
		t.Fatal(err)
	}
	protocolServer, err := loader.NewRuntimeHostProtocolServer(loader.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend, MaxRuntimes: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dialer, _ := startRuntimeHost(t, protocolServer)
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: loader.ModeHosted, Address: "unix:/runtime-host-wiring.sock", Dialer: dialer,
		VerifyInstalled: catalog.VerifyRuntime,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), records[0].Runtime); err != nil {
		t.Fatal(err)
	}
	// 预热触发完整装载（验证 → 协议 Describe/Start/Health），编译失败内核拒绝就绪。
	if err := manager.Warmup(context.Background(), []string{testRuntimeID}, 1); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Handler(testRuntimeID)(
		context.Background(), toolRuntimeRequest(), json.RawMessage(`{"value":"hello"}`),
	)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("unmarshal result %q: %v", result, err)
	}
	// 固件是通用信封回显：原样返回 payload，不做任何业务计算。
	if decoded["value"] != "hello" {
		t.Fatalf("result = %v, want original payload", decoded)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
