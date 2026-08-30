//go:build integration && unix

package loader_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"

	"google.golang.org/grpc"
)

const (
	helperEnabled   = "AILUO_RUNTIME_HELPER"
	helperSocket    = "AILUO_RUNTIME_SOCKET"
	helperMode      = "AILUO_RUNTIME_MODE"
	helperWriteFile = "AILUO_RUNTIME_WRITE_FILE"
)

type processHelperRuntime struct {
	runtimev1.UnimplementedRuntimeHostServer

	mode       string
	stop       chan struct{}
	ignoreStop bool
	stopOnce   sync.Once
}

func (s *processHelperRuntime) Describe(_ context.Context, request *runtimev1.DescribeRequest) (*runtimev1.RuntimeDescription, error) {
	return &runtimev1.RuntimeDescription{
		RuntimeId: request.Identity.RuntimeId, Version: request.Identity.Version, Mode: s.mode,
		SupportedProtocolVersions: []string{loader.RuntimeHostProtocolVersion},
	}, nil
}

func (s *processHelperRuntime) Start(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return readyResponse(request.Identity), nil
}

func (s *processHelperRuntime) Health(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return readyResponse(request.Identity), nil
}

func (s *processHelperRuntime) Invoke(_ context.Context, request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
	// 资源限额验证模式：向工作目录写入 8 KiB 文件，写失败（如 RLIMIT_FSIZE 生效）则报告失败。
	if target := os.Getenv(helperWriteFile); target != "" {
		if err := os.WriteFile(target, make([]byte, 8<<10), 0o600); err != nil {
			return &runtimev1.InvokeResponse{
				Identity: cloneIdentity(request.Identity), Success: false, ErrorCode: "file_write_failed",
			}, nil
		}
		return &runtimev1.InvokeResponse{
			Identity: cloneIdentity(request.Identity), Success: true, PayloadJson: []byte(`{"write":"ok"}`),
		}, nil
	}
	payload, _ := json.Marshal(map[string]any{
		"app_id": request.Context.AppId, "call_id": request.Context.CallId,
	})
	return &runtimev1.InvokeResponse{
		Identity: cloneIdentity(request.Identity), Success: true, PayloadJson: payload,
	}, nil
}

func (s *processHelperRuntime) Stop(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	if !s.ignoreStop {
		s.stopOnce.Do(func() { close(s.stop) })
	}
	return &runtimev1.LifecycleResponse{
		Identity: cloneIdentity(request.Identity), StatusCode: "stopped",
	}, nil
}

func TestIsolatedRuntimeHelper(t *testing.T) {
	if os.Getenv(helperEnabled) != "1" {
		return
	}
	socketPath := os.Getenv(helperSocket)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	implementation := &processHelperRuntime{
		mode: os.Getenv(helperMode), stop: make(chan struct{}), ignoreStop: os.Getenv("AILUO_RUNTIME_IGNORE_STOP") == "1",
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(512 << 10))
	runtimev1.RegisterRuntimeHostServer(server, implementation)
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	<-implementation.stop
	server.GracefulStop()
	_ = listener.Close()
	<-serveDone
}

func TestProcessHostRunsOutsideKernelAndShutsDownGracefully(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	socketPath := filepath.Join(workDir, "runtime.sock")
	spec := packagecontract.ProcessSpec{
		Path: executable, Args: []string{"-test.run=^TestIsolatedRuntimeHelper$"},
		Env: []string{
			helperEnabled + "=1", helperSocket + "=" + socketPath, helperMode + "=" + loader.ModeIsolated,
		},
		WorkDir: workDir, Address: "unix:" + socketPath,
	}
	var resolves atomic.Int32
	var verifies atomic.Int32
	host, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve: func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) {
			resolves.Add(1)
			return spec, nil
		},
		VerifyInstalled: func(_ context.Context, manifest loader.Manifest, resolved packagecontract.ProcessSpec) error {
			verifies.Add(1)
			if manifest.LockedDigest != digest || resolved.Path != executable {
				t.Fatalf("manifest=%#v spec=%#v", manifest, resolved)
			}
			return nil
		},
		DialTimeout: 3 * time.Second, StopGrace: time.Second, TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("isolated.real", loader.ModeIsolated)); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Handler("isolated.real")(
		context.Background(),
		contracts.RequestContext{
			AppID: "app.test", EchoID: "echo-1", RequestID: "request-1", CallID: "call-1",
			TargetType: "capability", CapabilityID: "test.capability", ServiceID: "test.service",
		},
		json.RawMessage(`{"value":1}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result, &decoded); err != nil ||
		decoded["app_id"] != "app.test" || decoded["call_id"] != "call-1" {
		t.Fatalf("result=%s err=%v", result, err)
	}
	// 三层校验：注册期 selectHost、加载期 loadRuntime、执行前 Load 内部 TOCTOU 防御各一次。
	if resolves.Load() != 3 || verifies.Load() != 3 {
		t.Fatalf("resolves=%d verifies=%d", resolves.Load(), verifies.Load())
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Load(context.Background(), runtimeManifest("isolated.after", loader.ModeIsolated)); err == nil {
		t.Fatal("closed process host accepted a new runtime")
	}
}

func TestProcessHostEnforcesFileSizeLimit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	socketPath := filepath.Join(workDir, "runtime.sock")
	writeTarget := filepath.Join(workDir, "out.bin")
	spec := packagecontract.ProcessSpec{
		Path: executable, Args: []string{"-test.run=^TestIsolatedRuntimeHelper$"},
		Env: []string{
			helperEnabled + "=1", helperSocket + "=" + socketPath, helperMode + "=" + loader.ModeIsolated,
			helperWriteFile + "=" + writeTarget,
		},
		WorkDir: workDir, Address: "unix:" + socketPath,
		// RLIMIT_FSIZE=1 KiB：helper 写入 8 KiB 必须被限额阻止。
		Limits: packagecontract.ProcessLimits{MaxFileBytes: 1024},
	}
	host, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve:     func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) { return spec, nil },
		Verify:      func(context.Context, loader.Manifest, packagecontract.ProcessSpec) error { return nil },
		DialTimeout: 3 * time.Second, StopGrace: time.Second, TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("isolated.limit", loader.ModeIsolated)); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Handler("isolated.limit")(
		context.Background(),
		contracts.RequestContext{
			AppID: "app.test", EchoID: "echo-1", RequestID: "request-1", CallID: "call-1",
			TargetType: "capability", CapabilityID: "test.capability", ServiceID: "test.service",
		},
		json.RawMessage(`{"value":1}`),
	)
	if err == nil {
		t.Fatal("file size limit was not enforced")
	}
	// 文件要么未被写入，要么写入被限额截断；超限写入必须失败。
	if info, statErr := os.Stat(writeTarget); statErr == nil && info.Size() > 1024 {
		t.Fatalf("limited process wrote %d bytes beyond RLIMIT_FSIZE", info.Size())
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestProcessHostForcesBoundedExitAfterStopGrace(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	socketPath := filepath.Join(workDir, "runtime.sock")
	spec := packagecontract.ProcessSpec{
		Path: executable, Args: []string{"-test.run=^TestIsolatedRuntimeHelper$"},
		Env: []string{
			helperEnabled + "=1", helperSocket + "=" + socketPath, helperMode + "=" + loader.ModeIsolated,
			"AILUO_RUNTIME_IGNORE_STOP=1",
		},
		WorkDir: workDir, Address: "unix:" + socketPath,
	}
	host, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve:        func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) { return spec, nil },
		Verify:         func(context.Context, loader.Manifest, packagecontract.ProcessSpec) error { return nil },
		DialTimeout:    3 * time.Second,
		StopGrace:      100 * time.Millisecond,
		TerminateGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("isolated.force", loader.ModeIsolated)); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "isolated.force"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("forced shutdown duration=%s", elapsed)
	}
}

func TestProcessHostRejectsUnsafeLaunchSpecifications(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	base := packagecontract.ProcessSpec{
		Path: executable, WorkDir: workDir, Address: "unix:" + filepath.Join(workDir, "runtime.sock"),
	}
	tests := []packagecontract.ProcessSpec{
		func() packagecontract.ProcessSpec { value := base; value.Path = "relative"; return value }(),
		func() packagecontract.ProcessSpec { value := base; value.WorkDir = "relative"; return value }(),
		func() packagecontract.ProcessSpec { value := base; value.Address = "192.0.2.1:9000"; return value }(),
		func() packagecontract.ProcessSpec {
			value := base
			value.Env = []string{"API_TOKEN=private"}
			return value
		}(),
		func() packagecontract.ProcessSpec {
			value := base
			value.Env = []string{"LD_PRELOAD=/tmp/inject.so"}
			return value
		}(),
		func() packagecontract.ProcessSpec {
			value := base
			value.Env = []string{"SAFE=1", "SAFE=2"}
			return value
		}(),
		func() packagecontract.ProcessSpec {
			value := base
			value.Args = []string{"bad\x00argument"}
			return value
		}(),
	}
	for _, spec := range tests {
		var verifies atomic.Int32
		host, err := loader.NewProcessHost(loader.ProcessHostConfig{
			Resolve: func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) { return spec, nil },
			Verify: func(context.Context, loader.Manifest, packagecontract.ProcessSpec) error {
				verifies.Add(1)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Verify(context.Background(), runtimeManifest("isolated.invalid", loader.ModeIsolated)); err == nil {
			t.Fatalf("unsafe spec accepted: %#v", spec)
		}
		if verifies.Load() != 0 {
			t.Fatalf("unsafe spec reached verifier: %#v", spec)
		}
	}
}
