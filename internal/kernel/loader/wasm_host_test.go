package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// hostedArtifact 读取 testdata 下的测试工件。
func hostedArtifact(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取测试工件 %s: %v", name, err)
	}
	return data
}

// hostedTestRequest 构造携带能力标识的治理上下文。
func hostedTestRequest(capabilityID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.test", EchoID: "echo-1", RequestID: "request-1",
		CapabilityID: capabilityID,
	}
}

func TestWasmHostRejectsInvalidConfiguration(t *testing.T) {
	if _, err := loader.NewWasmHost(loader.WasmHostConfig{}); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("NewWasmHost without read artifact error = %v, want ErrUnavailable", err)
	}
	// 宿主函数问题不再在构造期暴露：按清单提供，Verify/Load 期 fail-closed。
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(context.Context, loader.Manifest) ([]byte, error) { return nil, nil },
		HostFunctionsFor: staticHostFunctions(loader.HostedFunction{
			Module: "ailuo.host", Name: "echo",
		}),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	if err := host.Verify(context.Background(), loader.Manifest{
		ID: testPackageID, Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("Verify with incomplete host function error = %v, want ErrUnavailable", err)
	}
	duplicate := loader.HostedFunction{
		Module: "ailuo.host", Name: "echo",
		Call: func(context.Context, contracts.RequestContext, []byte) ([]byte, error) { return nil, nil },
	}
	host, err = loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:     func(context.Context, loader.Manifest) ([]byte, error) { return nil, nil },
		HostFunctionsFor: staticHostFunctions(duplicate, duplicate),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	if err := host.Verify(context.Background(), loader.Manifest{
		ID: testPackageID, Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}); !errors.Is(err, loader.ErrDuplicateID) {
		t.Fatalf("Verify with duplicate host function error = %v, want ErrDuplicateID", err)
	}
}

func TestWasmHostServesHostedArtifactThroughLoader(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("success", "success.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != testPackageID {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	ctx := context.Background()
	manifest := loader.Manifest{
		ID: testPackageID, Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}
	if err := manager.Register(ctx, manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := manager.EnsureLoaded(ctx, testPackageID); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if err := manager.EnsureLoaded(ctx, testPackageID); err != nil {
		t.Fatalf("已加载运行时应幂等返回：%v", err)
	}

	invoke := func(capabilityID string, payload string) (map[string]any, error) {
		t.Helper()
		lease, err := manager.Acquire(ctx, testPackageID)
		if err != nil {
			return nil, err
		}
		defer lease.Release()
		result, err := lease.Invoke(ctx, hostedTestRequest(capabilityID), json.RawMessage(payload))
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(result, &decoded); err != nil {
			return nil, fmt.Errorf("unmarshal result: %w", err)
		}
		return decoded, nil
	}

	echo, err := invoke(testInvokeCapabilityID, `{"value":"hello"}`)
	if err != nil {
		t.Fatalf("invoke %s: %v", testInvokeCapabilityID, err)
	}
	if echo["value"] != "hello" {
		t.Fatalf("echo result = %v, want original payload", echo)
	}
	// 载荷本身不是合法 JSON 属于协议违例，在进入 guest 之前被拒绝。
	// guest 显式拒绝路径的错误码映射由 wasm_host_internal_test 覆盖；
	// 固件不做工具路由，Loader 不测 guest 的分发行为。
	if _, err := invoke(testInvokeCapabilityID, `{"value":`); !errors.Is(err, loader.ErrRuntimeProtocol) {
		t.Fatalf("malformed payload error = %v, want ErrRuntimeProtocol", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestWasmHostHostFunctionProjectionBindsGovernedContext(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	seen := make(chan contracts.RequestContext, 1)
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctionsFor: staticHostFunctions(loader.HostedFunction{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
				seen <- request
				return body, nil
			},
		}),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	runtime, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
		HostFunctions: []packagecontract.HostedFunctionDecl{{Module: "ailuo.host", Name: "echo"}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer runtime.Stop(context.Background())
	invoker, ok := runtime.(loader.Invoker)
	if !ok {
		t.Fatal("hosted runtime must implement the capability Invoker")
	}

	payload := json.RawMessage(`{"echo":42}`)
	result, err := invoker.Invoke(context.Background(), hostedTestRequest("hostfn.echo"), payload)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(result) != `{"echo":42}` {
		t.Fatalf("host function echo result = %s, want payload echo", result)
	}
	select {
	case request := <-seen:
		if request.AppID != "app.test" || request.CapabilityID != "hostfn.echo" {
			t.Fatalf("host function received request = %+v, want app.test/hostfn.echo", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host function was not invoked")
	}
}

func TestWasmHostRejectsUndeclaredHostFunctionImport(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctionsFor: staticHostFunctions(loader.HostedFunction{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, _ contracts.RequestContext, body []byte) ([]byte, error) { return body, nil },
		}),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	// manifest 未声明宿主函数，但 hostfn 工件 import ailuo.host.echo → 加载期拒绝。
	if _, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.undeclared", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("Load with undeclared host function import error = %v, want ErrLoadFailed", err)
	}
}

func TestWasmHostVerifyRejectsUndeclaredHostFunction(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctionsFor: staticHostFunctions(loader.HostedFunction{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, _ contracts.RequestContext, body []byte) ([]byte, error) { return body, nil },
		}),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manifest := loader.Manifest{
		ID: "hostfn.verify", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
		HostFunctions: []packagecontract.HostedFunctionDecl{{Module: "ailuo.nonexistent", Name: "missing"}},
	}
	if err := host.Verify(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("Verify with undeclared host function error = %v, want ErrInvalidManifest", err)
	}
}

func TestWasmHostConcurrentInvocationsAreIsolated(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctionsFor: staticHostFunctions(loader.HostedFunction{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
				return json.Marshal(map[string]any{"app_id": request.AppID, "body": json.RawMessage(body)})
			},
		}),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	runtime, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.conc", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
		HostFunctions: []packagecontract.HostedFunctionDecl{{Module: "ailuo.host", Name: "echo"}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer runtime.Stop(context.Background())
	invoker, ok := runtime.(loader.Invoker)
	if !ok {
		t.Fatal("hosted runtime must implement the capability Invoker")
	}

	const workers = 8
	var group sync.WaitGroup
	failures := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			appID := fmt.Sprintf("app.%d", index)
			request := hostedTestRequest("hostfn.echo")
			request.AppID = appID
			payload := json.RawMessage(fmt.Sprintf(`{"worker":%d}`, index))
			result, err := invoker.Invoke(context.Background(), request, payload)
			if err != nil {
				failures <- err
				return
			}
			var decoded struct {
				AppID string          `json:"app_id"`
				Body  json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal(result, &decoded); err != nil {
				failures <- err
				return
			}
			if decoded.AppID != appID || string(decoded.Body) != string(payload) {
				failures <- fmt.Errorf("isolated call %d mismatched: app_id=%s body=%s", index, decoded.AppID, decoded.Body)
			}
		}(index)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

func TestWasmHostEnforcesMemoryLimit(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("success", "success.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:     func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		MemoryLimitPages: 1, // 64 KiB：低于 Go 运行时初始内存，编译阶段即拒绝
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manifest := loader.Manifest{
		ID: "memory.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}
	// 编译阶段强制线性内存上限：超限工件无法加载。
	if _, err := host.Load(context.Background(), manifest); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("Load under memory limit error = %v, want ErrLoadFailed", err)
	}
}

func TestWasmHostRejectsOversizedArtifact(t *testing.T) {
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:     func(_ context.Context, _ loader.Manifest) ([]byte, error) { return make([]byte, 4096), nil },
		MaxArtifactBytes: 1024,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manifest := loader.Manifest{
		ID: "oversized.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	}
	if err := host.Verify(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("Verify error = %v, want ErrInvalidManifest", err)
	}
	if _, err := host.Load(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("Load error = %v, want ErrInvalidManifest", err)
	}
}

func TestWasmHostTerminatesRunawayGuest(t *testing.T) {
	// 死循环 guest：不开启执行时间预算的话会永远占用 worker。
	artifact := hostedArtifact(t, filepath.Join("busy", "busy.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != "busy.test" {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
		CallTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	runtime, err := host.Load(context.Background(), loader.Manifest{
		ID: "busy.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleProvider, LockedDigest: digest,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer runtime.Stop(context.Background())
	invoker, ok := runtime.(loader.Invoker)
	if !ok {
		t.Fatal("hosted runtime must implement the capability Invoker")
	}
	started := time.Now()
	_, err = invoker.Invoke(context.Background(), hostedTestRequest("busy.loop"), json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Invoke error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("预算终止耗时 %v，死循环未被强制终止", elapsed)
	}
}
