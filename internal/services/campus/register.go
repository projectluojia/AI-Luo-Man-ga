package campus

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/bus"
)

// Host 返回 campus hosted 包的沙箱宿主（单一装配入口）：内置 wasm 工件经进程内
// 沙箱执行，权威存储经宿主函数投影（App 隔离在宿主侧强制），业务治理与既有
// Dispatcher 链路不变。main 与测试装配共用，避免重复的边界构造逻辑。
func Host(store bus.Store) (*loader.WasmHost, error) {
	return loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  ReadArtifact,
		HostFunctions: HostedFunctions(store),
	})
}

// Record 返回 campus 的安装清单（运行时 + Tool + Service + Capability 的单一来源），
// 供统一 Loader 注册内置包使用。
func Record() loader.InstalledRecord {
	return loader.InstalledRecord{
		Runtime:      Manifest(),
		Tools:        bus.ToolSpecs(),
		Service:      ServiceSpec(),
		Capabilities: CapabilitySpecs(),
	}
}
