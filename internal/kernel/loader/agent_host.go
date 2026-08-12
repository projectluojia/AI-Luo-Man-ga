package loader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentRuntimeID 是内核内置 AI 执行者（Python Agent）的稳定标识。
// agent 是唯一面向模型的 Capability 消费者：不注册任何能力，工具集由 Go 按 Run 投影。
const AgentRuntimeID = "ailuo.agent"

// agentBuiltinVersion 是内置 agent 运行时的版本。
const agentBuiltinVersion = "1.0.0"

// agentBuiltinDigest 锁定内置 agent 的运行时契约（进程模块 + 协议版本）：
// 协议升级会自然改变 digest，防止与旧协议误配。
var agentBuiltinDigest = func() string {
	sum := sha256.Sum256([]byte("ailuo.agent built-in isolated agent runtime\nprotocol " + agentprotocol.Version))
	return hex.EncodeToString(sum[:])
}()

// DefaultAgentPythonPath 返回 uv 管理的 Python 解释器路径（三平台一致），
// 供内核装配与集成测试解析内置 agent 进程路径。
func DefaultAgentPythonPath(projectRoot string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(projectRoot, "agent", ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(projectRoot, "agent", ".venv", "bin", "python")
}

// AgentSpec 是内置 agent 进程的启动规格，由内核装配提供。
type AgentSpec struct {
	PythonPath string
	WorkDir    string
	Address    string
	Env        []string
	Limits     ProcessLimits
}

// AgentHostConfig 装配内置 AI 执行者宿主。
type AgentHostConfig struct {
	// Resolve 返回 agent 进程启动规格（python 路径、工作目录、监听地址、限额）。
	Resolve func(context.Context) (AgentSpec, error)
	// Spawn 为 true 时内核负责启动与监督 agent 进程；false 表示连接运维已启动的 agent
	//（内核不拥有进程，仅连接其 gRPC 地址）。
	Spawn bool
	// Model 是加载期健康检查使用的模型标识（协议协商 + Provider 就绪）。
	Model string
	// Stdout / Stderr 是 agent 进程的输出目标（仅 Spawn 时使用；agent 的 Python 日志
	// 透传内核输出，与 installed 扩展的丢弃策略不同）。
	Stdout, Stderr io.Writer
	DialTimeout    time.Duration
	StopGrace      time.Duration
	TerminateGrace time.Duration
}

// AgentHost 以 isolated 形态监督内置 AI 执行者：进程与内置扩展包同构，
// 启动/限额/清理复用 loader 进程原语（startCommandProcess/applyProcessLimits/
// terminate/kill），不再需要专用宿主代码。
type AgentHost struct {
	config   AgentHostConfig
	manifest Manifest
}

// NewAgentHost 构造内置 agent 宿主；配置非法时返回显式错误。
func NewAgentHost(config AgentHostConfig) (*AgentHost, error) {
	if config.Resolve == nil || config.Model == "" {
		return nil, ErrUnavailable
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.StopGrace == 0 {
		config.StopGrace = 5 * time.Second
	}
	if config.TerminateGrace == 0 {
		config.TerminateGrace = 2 * time.Second
	}
	if !validProcessDuration(config.DialTimeout) || !validProcessDuration(config.StopGrace) ||
		!validProcessDuration(config.TerminateGrace) {
		return nil, ErrInvalidManifest
	}
	return &AgentHost{
		config: config,
		manifest: Manifest{
			ID: AgentRuntimeID, Version: agentBuiltinVersion, Mode: ModeIsolated,
			LockedDigest: agentBuiltinDigest, Pin: true,
		},
	}, nil
}

// Manifest 返回内置 agent 清单（pinned：常驻不随 IdleTTL 卸载）。
func (h *AgentHost) Manifest() Manifest {
	return h.manifest
}

// Verify 要求 manifest 与内置清单精确一致；任何字段不一致都拒绝加载。
func (h *AgentHost) Verify(_ context.Context, manifest Manifest) error {
	if manifest.ID != h.manifest.ID || manifest.Version != h.manifest.Version ||
		manifest.Mode != h.manifest.Mode || manifest.LockedDigest != h.manifest.LockedDigest {
		return ErrDescribeMismatch
	}
	return nil
}

// Load 启动（或连接）agent 进程并建立 agent 协议连接，返回 agentRuntime。
// 启动或连接失败一律 fail-closed：未就绪的 agent 不允许进入 Ready。
func (h *AgentHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
	if err := h.Verify(ctx, manifest); err != nil {
		return nil, err
	}
	spec, err := h.config.Resolve(ctx)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := validateAgentSpec(spec, h.config.Spawn); err != nil {
		return nil, err
	}
	var process *commandProcess
	if h.config.Spawn {
		process, err = startCommandProcess(ctx, ProcessSpec{
			Path:    spec.PythonPath,
			Args:    []string{"-m", "agent.runtime", "--listen", spec.Address},
			Env:     spec.Env,
			WorkDir: spec.WorkDir,
			Address: spec.Address,
			Limits:  spec.Limits,
		}, h.config.Stdout, h.config.Stderr)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	connection, client, err := dialAgent(ctx, spec.Address, process, h.config.DialTimeout)
	if err != nil {
		if process != nil {
			_ = reapAgentProcess(process, h.config.TerminateGrace)
		}
		return nil, errors.Join(ErrUnavailable, err)
	}
	return &agentRuntime{
		id: manifest.ID, version: manifest.Version,
		process: process, connection: connection, client: client,
		model: h.config.Model, stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}, nil
}

// validateAgentSpec 校验 agent 进程规格：监听地址必须是 loopback、限额在合理上限内；
// Spawn 模式额外要求绝对 python 路径与可选绝对工作目录。
func validateAgentSpec(spec AgentSpec, spawn bool) error {
	if !IsLocalRuntimeAddress(spec.Address) || !validProcessLimits(spec.Limits) {
		return ErrInvalidProcessSpec
	}
	if spawn {
		if !filepath.IsAbs(spec.PythonPath) || filepath.Clean(spec.PythonPath) != spec.PythonPath {
			return ErrInvalidProcessSpec
		}
		if spec.WorkDir != "" && (!filepath.IsAbs(spec.WorkDir) || filepath.Clean(spec.WorkDir) != spec.WorkDir) {
			return ErrInvalidProcessSpec
		}
	}
	return nil
}

// dialAgent 连接 agent 协议（agent.proto），与 isolated 扩展的 runtime_host.proto 不同。
// 进程退出（Spawn 模式）会取消拨号，避免连接永不返回。
func dialAgent(ctx context.Context, address string, process *commandProcess, dialTimeout time.Duration) (*grpc.ClientConn, agentv1.AgentRuntimeClient, error) {
	dialContext, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	monitorDone := make(chan struct{})
	if process != nil {
		go func() {
			select {
			case <-process.done:
				cancel()
			case <-monitorDone:
			}
		}()
	}
	connection, err := grpc.DialContext(
		dialContext,
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(agentprotocol.MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(agentprotocol.MaxGRPCMessageBytes),
		),
	)
	if process != nil {
		close(monitorDone)
	}
	if err != nil {
		return nil, nil, err
	}
	return connection, agentv1.NewAgentRuntimeClient(connection), nil
}

// reapAgentProcess 在加载失败时回收 agent 进程：强制终止、等待退出并释放限额。
func reapAgentProcess(process *commandProcess, terminateGrace time.Duration) error {
	if process.exited() {
		return nil
	}
	if err := killCommandProcess(process.command.Process); err != nil && !process.exited() {
		return ErrProcessCleanup
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), terminateGrace)
	defer cancel()
	select {
	case <-process.done:
	case <-cleanupContext.Done():
		return ErrProcessCleanup
	}
	if process.release != nil {
		_ = process.release()
		process.release = nil
	}
	return nil
}

// agentRuntime 是内置 AI 执行者的执行单元：监督 agent 进程，向内核暴露 agent 协议
// 客户端。生命周期（优雅停止 → 宽限等待 → 强制清理）与 processRuntime 同构，
// 但 agent 没有 RuntimeHost 协议 Stop，也不产生 Unix socket。
type agentRuntime struct {
	id, version    string
	process        *commandProcess // Spawn=false 时为 nil
	connection     *grpc.ClientConn
	client         agentv1.AgentRuntimeClient
	model          string
	stopGrace      time.Duration
	terminateGrace time.Duration

	stopOnce sync.Once
	stopErr  error
}

func (r *agentRuntime) Describe(context.Context) (Description, error) {
	return Description{ID: r.id, Version: r.version, Mode: ModeIsolated}, nil
}

func (r *agentRuntime) Start(context.Context) error { return nil }

func (r *agentRuntime) Health(ctx context.Context) error {
	if r.process != nil && r.process.exited() {
		return ErrUnavailable
	}
	return health.AgentChecker{Client: r.client, Model: r.model}.Ping(ctx)
}

// Invoke 不服务请求/响应调用：agent 是 Capability 消费者，不注册任何能力。
func (r *agentRuntime) Invoke(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return nil, ErrUnsupportedMode
}

// Stop 优雅停止 agent：Spawn 模式发送终止信号等待退出，超时强制清理；连接模式只关闭连接。
func (r *agentRuntime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { r.stopErr = r.stop(ctx) })
	return r.stopErr
}

func (r *agentRuntime) stop(ctx context.Context) error {
	if r.process == nil {
		return r.closeTransport()
	}
	if r.process.exited() {
		return r.finish()
	}
	if err := terminateCommandProcess(r.process.command.Process); err != nil && !r.process.exited() {
		// 无法优雅终止：转入强制清理。
	} else if r.process.wait(ctx, r.stopGrace) {
		return r.finish()
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.stopGrace+r.terminateGrace+time.Second)
	defer cancel()
	if err := r.forceCleanup(cleanupContext); err != nil {
		return errors.Join(ErrProcessCleanup, err)
	}
	return r.finish()
}

func (r *agentRuntime) forceCleanup(ctx context.Context) error {
	if r.process.exited() {
		return r.finish()
	}
	if err := killCommandProcess(r.process.command.Process); err != nil && !r.process.exited() {
		return ErrProcessCleanup
	}
	if !r.process.wait(ctx, r.terminateGrace) {
		return ErrProcessCleanup
	}
	return r.finish()
}

func (r *agentRuntime) finish() error {
	if r.process != nil && r.process.release != nil {
		_ = r.process.release()
		r.process.release = nil
	}
	return r.closeTransport()
}

func (r *agentRuntime) closeTransport() error {
	if r.connection == nil {
		return nil
	}
	connection := r.connection
	r.connection = nil
	return connection.Close()
}

// AgentClient 返回 agent 协议客户端，供内核 Orchestrator 驱动模型循环。
func (r *agentRuntime) AgentClient() agentv1.AgentRuntimeClient {
	return r.client
}

// Done 返回 agent 进程退出通知（连接模式永不触发）。
func (r *agentRuntime) Done() <-chan struct{} {
	if r.process == nil {
		return nil
	}
	return r.process.done
}

// Err 返回 agent 进程退出错误。
func (r *agentRuntime) Err() error {
	if r.process == nil {
		return nil
	}
	return r.process.waitErr
}

// AgentClientProvider 是持有 agent 协议客户端的运行时的窄接口，供内核装配断言。
type AgentClientProvider interface {
	AgentClient() agentv1.AgentRuntimeClient
}

// AgentProcessLifecycle 是 agent 运行时的进程生命周期窄接口，供内核装配做崩溃监控。
type AgentProcessLifecycle interface {
	Done() <-chan struct{}
	Err() error
}

var (
	_ Runtime               = (*agentRuntime)(nil)
	_ AgentClientProvider   = (*agentRuntime)(nil)
	_ AgentProcessLifecycle = (*agentRuntime)(nil)
)
