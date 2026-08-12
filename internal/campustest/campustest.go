// Package campustest 提供校园服务 hosted 装配的共享测试辅助，
// 供 campus、echo、e2e 等包以真实 hosted 链路验证能力。
package campustest

import (
	"context"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

// RegisterHosted 以 hosted 包形态装配校园服务：内置 wasm 工件经进程内沙箱执行，
// 权威存储经宿主函数投影；装配完成即预热 pin 包（编译在测试装配时完成，
// 避免占用 Run 的 deadline）。
func RegisterHosted(t testing.TB, target *registry.Registry, store bus.Store) {
	t.Helper()
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  campus.ReadArtifact,
		HostFunctions: campus.HostedFunctions(store),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownContext); err != nil {
			t.Errorf("loader shutdown: %v", err)
		}
	})
	record := loader.InstalledRecord{
		Runtime:      campus.Manifest(),
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
	}
	if err := loader.RegisterInstalled(manager, target, []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	warmupContext, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Warmup(warmupContext, []string{campus.ServiceID}, 1); err != nil {
		t.Fatalf("warm campus hosted package: %v", err)
	}
}
