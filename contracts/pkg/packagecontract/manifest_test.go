package packagecontract_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

func testCapability(id string) capability.CapabilitySpec {
	return capability.CapabilitySpec{
		ID: id, Version: "1.0.0", Name: id,
		InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "test.resource"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
	}
}

func testManifest() packagecontract.Manifest {
	return packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "test.pkg", Version: "1.0.0",
		Capabilities: []capability.CapabilitySpec{testCapability("test.query")},
		Components: []packagecontract.Component{{
			ID: "provider", Mode: packagecontract.ModeHosted, Role: packagecontract.RoleProvider,
			Entrypoint: "provider.wasm", Exports: []string{"test.query"},
		}},
	}
}

func TestValidateManifestAcceptsStructuredCapability(t *testing.T) {
	manifest := testManifest()
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManifestRejectsInvalidCapabilityContract(t *testing.T) {
	for name, mutate := range map[string]func(*packagecontract.Manifest){
		"invalid resource": func(m *packagecontract.Manifest) { m.Capabilities[0].Authorization.ResourceType = "bad type" },
		"invalid replay":   func(m *packagecontract.Manifest) { m.Capabilities[0].Execution.Replay = "maybe" },
		"unknown capability": func(m *packagecontract.Manifest) {
			m.Capabilities = append(m.Capabilities, testCapability("test.hidden"))
		},
		"duplicate export": func(m *packagecontract.Manifest) {
			m.Components = append(m.Components, packagecontract.Component{
				ID: "second", Mode: packagecontract.ModeHosted, Role: packagecontract.RoleProvider,
				Entrypoint: "second.wasm", Exports: []string{"test.query"},
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := testManifest()
			mutate(&manifest)
			if err := packagecontract.ValidateManifest(manifest); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestValidateManifestRejectsInvalidStorageAndProcess(t *testing.T) {
	manifest := testManifest()
	manifest.Storage = &packagecontract.Storage{Namespace: "other/data"}
	if err := packagecontract.ValidateManifest(manifest); err == nil {
		t.Fatal("foreign storage namespace accepted")
	}
	if err := packagecontract.ValidateProcessTemplate(packagecontract.ProcessTemplate{Path: filepath.Join("..", "runner")}); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("invalid process error=%v", err)
	}
}

func TestValidateLockMatchesComponentArtifacts(t *testing.T) {
	manifest := testManifest()
	artifactPath := filepath.Join(t.TempDir(), "provider.wasm")
	lock := packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: manifest.ID, PackageVersion: manifest.Version,
		ManifestSHA256: strings.Repeat("a", 64), Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: "provider", Path: artifactPath, SHA256: strings.Repeat("b", 64),
		}},
	}
	if err := packagecontract.ValidateLock(lock, manifest); err != nil {
		t.Fatal(err)
	}
	// hosted 组件不得携带进程规格。cloneLock 深拷贝 Artifacts：结构体复制共用
	// 底层数组，直接改 bad.Artifacts[0] 会同时改掉上面已校验过的 lock。
	cloneLock := func() packagecontract.Lock {
		copied := lock
		copied.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
		for index, artifact := range copied.Artifacts {
			if artifact.Process != nil {
				process := *artifact.Process
				process.Args = append([]string(nil), artifact.Process.Args...)
				copied.Artifacts[index].Process = &process
			}
		}
		return copied
	}
	bad := cloneLock()
	bad.Artifacts[0].Process = &packagecontract.ProcessSpec{Path: filepath.Join(t.TempDir(), "runner"), Address: "127.0.0.1:9001"}
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted hosted component with process spec")
	}
	// lock 的工件文件名必须与清单 entrypoint 一致：否则摘要可以绑到包目录外
	// 任意一个绝对路径文件上，装载的就不是清单声明的工件。
	bad = cloneLock()
	bad.Artifacts[0].Path = filepath.Join(t.TempDir(), "other.wasm")
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted artifact path not matching entrypoint")
	}
	// 摘要必须是合法十六进制：只校验长度会让 64 个 "g" 混过完整性校验前置检查。
	bad = cloneLock()
	bad.Artifacts[0].SHA256 = strings.Repeat("g", 64)
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex artifact digest")
	}
	bad = cloneLock()
	bad.ManifestSHA256 = strings.Repeat("g", 64)
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex manifest digest")
	}
}

func TestValidateProcessSpecRejectsNonLoopback(t *testing.T) {
	spec := packagecontract.ProcessSpec{Address: "192.0.2.1:9000"}
	if err := packagecontract.ValidateProcessSpec(spec); err == nil {
		t.Fatal("ValidateProcessSpec accepted non-loopback address")
	}
}

func TestValidateProcessTemplateRejectsUnixAddressOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 使用该进程地址策略")
	}
	if err := packagecontract.ValidateProcessTemplate(packagecontract.ProcessTemplate{
		Path: "runner", Address: "unix:/runtime.sock",
	}); err == nil {
		t.Fatal("ValidateProcessTemplate accepted Unix address on Windows")
	}
}

func TestDecodeStrictJSONRejectsProcessEnvironment(t *testing.T) {
	var spec packagecontract.ProcessSpec
	if err := packagecontract.DecodeStrictJSON([]byte(`{"env":[]}`), &spec); err == nil {
		t.Fatal("DecodeStrictJSON accepted removed process environment field")
	}
}

func TestLocalRuntimeAddressPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9000", "[::1]:9000", "unix:/runtime.sock"} {
		if !packagecontract.IsLocalRuntimeAddress(address) {
			t.Errorf("IsLocalRuntimeAddress(%q) = false", address)
		}
	}
	for _, address := range []string{"0.0.0.0:9000", "192.0.2.1:9000", "localhost:9000", "unix:relative.sock", "unix:/runtime/../socket.sock"} {
		if packagecontract.IsLocalRuntimeAddress(address) {
			t.Errorf("IsLocalRuntimeAddress(%q) = true", address)
		}
	}
}

func TestSchemaVersionConstantIsNeutral(t *testing.T) {
	if packagecontract.SchemaVersion != "ailuo.package.v3" {
		t.Fatalf("SchemaVersion = %q, want ailuo.package.v3", packagecontract.SchemaVersion)
	}
}

func TestIsPackagePathIsPlatformNeutral(t *testing.T) {
	for _, valid := range []string{".", "guest", "guest/main.ts"} {
		if !packagecontract.IsPackagePath(valid) {
			t.Errorf("IsPackagePath(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "../outside", `..\outside`, `C:\\pkg\\main.wasm`, "/tmp/main.wasm", "guest//main.ts", "guest/./main.ts"} {
		if packagecontract.IsPackagePath(invalid) {
			t.Errorf("IsPackagePath(%q) = true", invalid)
		}
	}
}

func TestValidateDependencyRejectsMalformedConstraint(t *testing.T) {
	if err := packagecontract.ValidateDependency(packagecontract.Dependency{ID: "bus.query", Constraint: "not-a-constraint"}); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("ValidateDependency error = %v, want ErrInvalidFormat", err)
	}
}

func TestValidateSourceRequiresExplicitScheme(t *testing.T) {
	for _, source := range []string{"path:packages/demo", "github:owner/repo"} {
		if err := packagecontract.ValidateSource(source); err != nil {
			t.Fatalf("ValidateSource(%q): %v", source, err)
		}
	}
	for _, source := range []string{
		"", "packages/demo", "path:../demo", "github:owner", "github:owner/repo/extra",
		"github:./repo", "github:owner/..",
	} {
		if err := packagecontract.ValidateSource(source); !errors.Is(err, packagecontract.ErrInvalidFormat) {
			t.Fatalf("ValidateSource(%q) = %v, want ErrInvalidFormat", source, err)
		}
	}
}

// module 与 name 都允许含 `.`，因此 {"ailuo.bus","query"} 与 {"ailuo","bus.query"}
// 是两个不同声明；用 `.` 拼键会让它们撞成同一个键并被误判为重复声明。
func TestHostedFunctionKeyDistinguishesDottedSegments(t *testing.T) {
	decls := []packagecontract.HostedFunctionDecl{
		{Module: "ailuo.bus", Name: "query"},
		{Module: "ailuo", Name: "bus.query"},
	}
	if err := packagecontract.ValidateHostedFunctions(decls); err != nil {
		t.Fatalf("ValidateHostedFunctions: %v", err)
	}
	if packagecontract.HostedFunctionKey("ailuo.bus", "query") == packagecontract.HostedFunctionKey("ailuo", "bus.query") {
		t.Fatal("HostedFunctionKey 对歧义的 module/name 切分产生了相同键")
	}
}
