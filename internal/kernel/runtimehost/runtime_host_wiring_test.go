//go:build unix

package runtimehost_test

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
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader/wasmhost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtimehost"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testRuntimeID 是 Runtime Host 集成测试包内运行组件的稳定标识：
// Runtime Manifest ID 与 Package ID 不同名，与 testcontract_test.go 的
// hosted 常量构成同一测试包的两条身份。
const testRuntimeID = testPackageID + ".runtime"

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
	installed := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: testPackageID, Version: "1.0.0",
		Pin: true,
		Capabilities: []capability.CapabilitySpec{{
			ID: testCapabilityID, Version: "1.0.0", Name: "测试回显",
			Description:     "测试回显",
			InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
			Authorization:   capability.AuthorizationSpec{ResourceType: "capability.resource"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		}},
		Components: []packagecontract.Component{{
			ID: "runtime", Mode: loader.ModeHosted, ABIVersion: packagecontract.GuestABI1, Role: packagecontract.RoleProvider, Entrypoint: "success.wasm",
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

	// 安装一个不在项目锁中的合法 hosted 包，验证 Runtime Host 不会因安装根可见而加载它。
	unlistedID := "runtime.unlisted"
	unlistedDirectory := filepath.Join(root, unlistedID)
	if err := os.Mkdir(unlistedDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	unlistedArtifactPath := filepath.Join(unlistedDirectory, "success.wasm")
	if err := os.WriteFile(unlistedArtifactPath, artifactBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	unlistedManifestValue := installed
	unlistedManifestValue.ID = unlistedID
	unlistedManifest, err := json.Marshal(unlistedManifestValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unlistedDirectory, "manifest.json"), unlistedManifest, 0o640); err != nil {
		t.Fatal(err)
	}
	unlistedManifestDigest := sha256.Sum256(unlistedManifest)
	unlistedLock := lock
	unlistedLock.PackageID = unlistedID
	unlistedLock.ManifestSHA256 = hex.EncodeToString(unlistedManifestDigest[:])
	unlistedLock.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
	unlistedLock.Artifacts[0].Path = unlistedArtifactPath
	unlistedLockBytes, err := json.Marshal(unlistedLock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unlistedDirectory, "lock.json"), unlistedLockBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	// 项目锁就地将本测试包锁定进项目：helper 属于 loader 包私有测试，跨包
	// 复制任意 ID 形态只会制造第二份实现，这里只需单包锁定。
	manifestPath := filepath.Join(t.TempDir(), "ailuo.toml")
	if err := os.WriteFile(manifestPath, []byte("[project]\nid = \"test\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	projectManifestSHA, err := packageio.HashFile(t.Context(), manifestPath, packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := packageio.HashFile(t.Context(), filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	var packageLock packagecontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &packageLock); err != nil {
		t.Fatal(err)
	}
	lockSHA, err := packageio.CanonicalLockDigest(t.Context(), directory, packageLock)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.DiscoverLocked(t.Context(), projectcontract.Lock{
		SchemaVersion:         projectcontract.SchemaVersion,
		ProjectID:             "test",
		ProjectManifestSHA256: projectManifestSHA,
		Packages: []projectcontract.LockedPackage{{
			ID: testPackageID, Version: "1.0.0", Source: "path:packages/" + testPackageID,
			ManifestSHA256: manifestSHA, LockSHA256: lockSHA,
		}},
	})
	if err != nil || len(records) != 1 || records[0].Runtime.ID != testRuntimeID {
		t.Fatalf("discover records=%#v err=%v", records, err)
	}
	backend, err := runtimehost.NewHostedRuntimeBackend(wasmhost.WasmHostConfig{ReadArtifact: catalog.ReadArtifact})
	if err != nil {
		t.Fatal(err)
	}
	protocolServer, err := runtimehost.NewRuntimeHostProtocolServer(runtimehost.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend,
		AllowedRuntimes: []runtimehost.BackendIdentity{{ID: records[0].Runtime.ID, Version: records[0].Runtime.Version}},
		MaxRuntimes:     1, MaxConcurrent: 1,
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
		context.Background(), capabilityRuntimeRequest(), json.RawMessage(`{"value":"hello"}`),
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
	_, err = protocolServer.Describe(context.Background(), &runtimev1.DescribeRequest{
		Identity: &runtimev1.RuntimeIdentity{
			RuntimeId: unlistedID + ".runtime", Version: "1.0.0",
			ProtocolVersion: loader.RuntimeHostProtocolVersion,
		},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unlisted runtime describe error=%v, want NotFound", err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
