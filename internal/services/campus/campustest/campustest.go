// Package campustest 提供校园服务 hosted 装配的共享测试辅助，
// 供 campus、echo、e2e 等包以真实 hosted 链路验证能力。
//
// 装配形态按平台拆分（Unix 走真实安装目录发现，非 Unix 内存构造清单，均经
// 进程内 WasmHost + 宿主函数投影）：本文件只含跨平台共用的注册与预热逻辑。
package campustest

import (
	"context"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
)

// registerHosted 注册并预热已发现记录（Unix 与非 Unix 共用）。
func registerHosted(t testing.TB, target *registry.Registry, host loader.Host, records []loader.InstalledRecord) {
	t.Helper()
	manager, err := loader.New(host)
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
	if err := loader.RegisterInstalled(context.Background(), manager, target, records); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	warmupContext, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Warmup(warmupContext, []string{campus.ServiceID}, 1); err != nil {
		t.Fatalf("warm campus hosted package: %v", err)
	}
}
