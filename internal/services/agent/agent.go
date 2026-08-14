// Package agent 以插件形态提供内置 AI 执行者（Python Agent）：运行实现经
// internal/kernel/loader 的 Host 接口以 isolated Runtime 形态受监督（进程
// 启动、资源限额、健康检查与优雅清理复用 loader 进程原语），经 agent.Record
// 与 campus/installed 包同一 RegisterInstalled 路径注册。内核只通过
// internal/kernel/executor 契约使用它（ClientProvider/ProcessLifecycle），不依赖本包。
// 对外核心能力为 agent.run（受治理的 child Run/Subagent），由 Register 在
// 内核 Orchestrator 装配完成后单独注册。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

const (
	// RuntimeID 是内置 AI 执行者运行时的稳定标识（Loader 中的实现单元）。
	RuntimeID = "ailuo.agent"
	// ServiceID 是 Agent Service 的注册标识（Registry 中的能力面单元）。
	ServiceID = "agent"
	// CapabilityID 是 Agent Service 对外核心能力：创建受治理的 child Run。
	// 该标识由内核 child-Run 契约（echo.SubagentCapabilityID）定义，
	// Agent Service 是其实现方。
	CapabilityID = echo.SubagentCapabilityID
	// runtimeVersion 是内置 agent 运行时的版本。
	runtimeVersion = "1.0.0"
)

// runtimeDigest 锁定内置 agent 的运行时契约（进程模块 + 协议版本）：
// 协议升级会自然改变 digest，防止与旧协议误配。
var runtimeDigest = func() string {
	sum := sha256.Sum256([]byte("ailuo.agent built-in isolated agent runtime\nprotocol " + executor.Version))
	return hex.EncodeToString(sum[:])
}()

// DefaultPythonPath 返回 uv 管理的 Python 解释器路径（三平台一致），
// 供内核装配与集成测试解析内置 agent 进程路径。
func DefaultPythonPath(projectRoot string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(projectRoot, "agent", ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(projectRoot, "agent", ".venv", "bin", "python")
}

// Record 返回内置 agent 运行时的安装清单（单一来源：与宿主清单一致），
// 供统一 Loader 以与 campus/installed 相同的 RegisterInstalled 路径注册。
// Agent Service（agent.run）依赖内核 Orchestrator，由 Register 在装配完成后
// 单独注册，因此本记录只携带 Runtime 清单。
func Record(host *Host) loader.InstalledRecord {
	return loader.InstalledRecord{Runtime: host.Manifest()}
}
