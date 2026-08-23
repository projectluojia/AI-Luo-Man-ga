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

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

// stringToolArtifact 读取参考包编译产物；测试工作目录为 internal/kernel/loader。
func stringToolArtifact(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "extensions", "strings.tool", "strings.tool.wasm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 strings.tool 工件（请先运行 ailuo pack extensions/strings.tool）: %v", err)
	}
	return data
}

// hostedArtifact 读取 testdata 下的测试工件。
func hostedArtifact(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取测试工件 %s: %v", name, err)
	}
	return data
}

// hostedTestRequest 构造携带工具标识的治理上下文。
func hostedTestRequest(toolID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.campus", EchoID: "echo-1", RequestID: "request-1",
		ToolID: toolID,
	}
}

// newStringToolHost 用 strings.tool 工件构造 WasmHost。
func newStringToolHost(t *testing.T, functions ...loader.HostedFunction) *loader.WasmHost {
	t.Helper()
	artifact := stringToolArtifact(t)
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != "strings.tool" {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
		HostFunctions: functions,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	return host
}

func TestWasmHostRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		config loader.WasmHostConfig
		want   error
	}{
		{name: "nil read artifact", config: loader.WasmHostConfig{}, want: loader.ErrUnavailable},
		{name: "bad host function", config: loader.WasmHostConfig{
			ReadArtifact:  func(context.Context, loader.Manifest) ([]byte, error) { return nil, nil },
			HostFunctions: []loader.HostedFunction{{Module: "ailuo.host", Name: "echo"}},
		}, want: loader.ErrUnavailable},
		{name: "duplicate host function", config: loader.WasmHostConfig{
			ReadArtifact: func(context.Context, loader.Manifest) ([]byte, error) { return nil, nil },
			HostFunctions: []loader.HostedFunction{
				{Module: "ailuo.host", Name: "echo", Call: func(context.Context, contracts.RequestContext, []byte) ([]byte, error) { return nil, nil }},
				{Module: "ailuo.host", Name: "echo", Call: func(context.Context, contracts.RequestContext, []byte) ([]byte, error) { return nil, nil }},
			},
		}, want: loader.ErrDuplicateID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loader.NewWasmHost(tc.config); !errors.Is(err, tc.want) {
				t.Fatalf("NewWasmHost error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWasmHostServesStringToolsThroughLoader(t *testing.T) {
	host := newStringToolHost(t)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	ctx := context.Background()
	manifest := loader.Manifest{
		ID: "strings.tool", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}
	if err := manager.Register(ctx, manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := manager.EnsureLoaded(ctx, "strings.tool"); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if err := manager.EnsureLoaded(ctx, "strings.tool"); err != nil {
		t.Fatalf("已加载运行时应幂等返回：%v", err)
	}

	invoke := func(toolID string, payload string) (map[string]any, error) {
		t.Helper()
		lease, err := manager.Acquire(ctx, "strings.tool")
		if err != nil {
			return nil, err
		}
		defer lease.Release()
		result, err := lease.Invoke(ctx, hostedTestRequest(toolID), json.RawMessage(payload))
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(result, &decoded); err != nil {
			return nil, fmt.Errorf("unmarshal result: %w", err)
		}
		return decoded, nil
	}

	length, err := invoke("strings.len", `{"value":"hello"}`)
	if err != nil {
		t.Fatalf("strings.len: %v", err)
	}
	if length["length"] != float64(5) {
		t.Fatalf("strings.len result = %v, want length 5", length)
	}
	joined, err := invoke("strings.join", `{"values":["a","b","c"],"separator":"-"}`)
	if err != nil {
		t.Fatalf("strings.join: %v", err)
	}
	if joined["value"] != "a-b-c" {
		t.Fatalf("strings.join result = %v, want a-b-c", joined)
	}
	upper, err := invoke("strings.upper", `{"value":"abc"}`)
	if err != nil {
		t.Fatalf("strings.upper: %v", err)
	}
	if upper["value"] != "ABC" {
		t.Fatalf("strings.upper result = %v, want ABC", upper)
	}

	// guest 显式拒绝：未知工具返回稳定错误，guest 错误码不进外部响应。
	if _, err := invoke("strings.missing", `{}`); !errors.Is(err, loader.ErrHostedCallRejected) {
		t.Fatalf("unknown tool error = %v, want ErrHostedCallRejected", err)
	}
	// 载荷本身不是合法 JSON 属于协议违例，在进入 guest 之前被拒绝。
	if _, err := invoke("strings.len", `{"value":`); !errors.Is(err, loader.ErrRuntimeProtocol) {
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
		HostFunctions: []loader.HostedFunction{{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
				seen <- request
				return body, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	runtime, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
		HostFunctions: []packmgr.HostedFunctionDecl{{Module: "ailuo.host", Name: "echo"}},
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
		if request.AppID != "app.campus" || request.ToolID != "hostfn.echo" {
			t.Fatalf("host function received request = %+v, want app.campus/hostfn.echo", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host function was not invoked")
	}
}

func TestWasmHostRejectsUndeclaredHostFunctionImport(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctions: []loader.HostedFunction{{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, _ contracts.RequestContext, body []byte) ([]byte, error) { return body, nil },
		}},
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	// manifest 未声明宿主函数，但 hostfn 工件 import ailuo.host.echo → 加载期拒绝。
	if _, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.undeclared", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("Load with undeclared host function import error = %v, want ErrLoadFailed", err)
	}
}

func TestWasmHostVerifyRejectsUndeclaredHostFunction(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctions: []loader.HostedFunction{{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, _ contracts.RequestContext, body []byte) ([]byte, error) { return body, nil },
		}},
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manifest := loader.Manifest{
		ID: "hostfn.verify", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
		HostFunctions: []packmgr.HostedFunctionDecl{{Module: "ailuo.nonexistent", Name: "missing"}},
	}
	if err := host.Verify(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("Verify with undeclared host function error = %v, want ErrInvalidManifest", err)
	}
}

func TestWasmHostConcurrentInvocationsAreIsolated(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("hostfn", "hostfn.wasm"))
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		HostFunctions: []loader.HostedFunction{{
			Module: "ailuo.host", Name: "echo",
			Call: func(_ context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
				return json.Marshal(map[string]any{"app_id": request.AppID, "body": json.RawMessage(body)})
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	runtime, err := host.Load(context.Background(), loader.Manifest{
		ID: "hostfn.conc", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
		HostFunctions: []packmgr.HostedFunctionDecl{{Module: "ailuo.host", Name: "echo"}},
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
	artifact := stringToolArtifact(t)
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:     func(_ context.Context, _ loader.Manifest) ([]byte, error) { return artifact, nil },
		MemoryLimitPages: 1, // 64 KiB：低于 Go 运行时初始内存，编译阶段即拒绝
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manifest := loader.Manifest{
		ID: "strings.memlimit", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
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
		ID: "oversized.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
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
	artifact := hostedArtifact(t, "busy.wasm")
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
		ID: "busy.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
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
