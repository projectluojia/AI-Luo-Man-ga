package runtimehost_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtimehost"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// Loader hosted 测试契约：宿主侧集中持有的稳定测试标识。
// 测试固件（testdata/{success,hostfn,busy}）不知道包名与能力名；如果测试需要
// 新增标识，在此集中声明，避免字面量在测试间漂移。
const (
	// testPackageID 是 hosted 测试包的 Package/Runtime 标识。
	testPackageID = "runtime.test"
	// testCapabilityID 是测试包对外暴露的能力标识。
	testCapabilityID = "runtime.test.echo.cap"
)

// digest 是测试清单锁定的占位工件摘要（ReadArtifact 侧校验）。
const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// startRuntimeHost 在 bufconn 上启动 RuntimeHost 协议服务，返回拨号器与拨号计数。
func startRuntimeHost(t *testing.T, implementation runtimev1.RuntimeHostServer) (func(context.Context, string) (net.Conn, error), *atomic.Int32) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(runtimehost.RuntimeHostGRPCServerOptions()...)
	runtimev1.RegisterRuntimeHostServer(server, implementation)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	var dials atomic.Int32
	return func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return listener.Dial()
	}, &dials
}

// newRuntimeGRPCHost 构造经 bufconn 拨号的 GRPCHost（VerifyInstalled 只计数）。
func newRuntimeGRPCHost(t *testing.T, mode string, dialer func(context.Context, string) (net.Conn, error), verifies *atomic.Int32) *loader.GRPCHost {
	t.Helper()
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: mode, Address: "unix:/runtime-host-test.sock", Dialer: dialer,
		VerifyInstalled: func(context.Context, loader.Manifest) error {
			verifies.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// runtimeManifest 构造 hosted 测试清单。
func runtimeManifest(id, mode string) loader.Manifest {
	return loader.Manifest{
		ID: id, Version: "1.2.3", Mode: mode, Role: loader.RoleProvider, LockedDigest: digest,
		ABIVersion: func() string {
			if len(packagecontract.SupportedGuestABIs) > 0 {
				return packagecontract.GuestABI1
			}
			return ""
		}(),
	}
}

// governedRuntimeRequest 构造通过协议校验的最小治理调用上下文。
func governedRuntimeRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.test", EchoID: "echo-1", RequestID: "request-1", TraceID: "trace-1",
		RunID: "run-1", ParentRunID: "parent-1", CallID: "call-1", CallDepth: 2,
		IdempotencyKey: "operation-1", ConfirmationID: "confirmation-1", ProtocolVersion: "1.0",
		CapabilityID: "test.capability", CallChain: []string{"first"},
	}
}

// hostedArtifact 读取 testdata 下的测试工件。
func hostedArtifact(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
