// Package agent 以 Service 形态纳管内置 AI 执行者（Python Agent），与 campus
// 等业务 Service 同级：运行实现经 internal/kernel/loader 的 Host 接口以
// isolated Runtime 形态受监督（进程启动、资源限额、健康检查与优雅清理复用
// loader 进程原语），对外核心能力为 agent.run（受治理的 child Run/Subagent）。
// loader 不包含任何 agent 特化代码，装配只在本包与内核接线点（main/e2e）出现。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
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
	sum := sha256.Sum256([]byte("ailuo.agent built-in isolated agent runtime\nprotocol " + agentprotocol.Version))
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

// ClientProvider 是持有 agent 协议客户端的运行时的窄接口，供内核装配断言。
type ClientProvider interface {
	AgentClient() agentv1.AgentRuntimeClient
}

// ProcessLifecycle 是 agent 运行时的进程生命周期窄接口，供内核装配做崩溃监控。
type ProcessLifecycle interface {
	Done() <-chan struct{}
	Err() error
}
