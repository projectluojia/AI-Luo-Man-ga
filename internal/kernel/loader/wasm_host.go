package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	// wasiModuleName 是 WASI 快照模块名：沙箱自动提供，包不得声明为宿主函数。
	wasiModuleName = "wasi_snapshot_preview1"
	// hostedHostFunctionError 是宿主函数调用失败时返回给 guest 的长度标记（-1 的无符号表示）。
	hostedHostFunctionError = 0xFFFFFFFF
	// hostedStdoutLimit 限制 guest 单次调用 stdout 输出字节数，防止无界输出。
	hostedStdoutLimit = 512 << 10
	// hostedDefaultMemoryPages 是默认 guest 线性内存上限（2048 页 = 128 MiB）。
	hostedDefaultMemoryPages = 2048
	// hostedDefaultMaxArtifactBytes 是默认工件字节上限。
	hostedDefaultMaxArtifactBytes = 32 << 20
	// hostedDefaultCallTimeout 是单次 hosted 调用的默认执行时间预算。wazero 编译期
	// 周期检查（WithCloseOnContextDone）在预算耗尽时强制终止 guest 并关闭模块，
	// 这是进程内沙箱可强制执行的 CPU 时间边界（wazero 无指令级计数）。
	hostedDefaultCallTimeout = 30 * time.Second
)

var (
	// ErrHostedCallRejected 表示 hosted 包显式拒绝了调用（guest 信封 ok=false）。
	// guest 提供的错误码与消息只进入内部日志，不进入外部响应。
	ErrHostedCallRejected = errors.New("hosted package rejected the call")
)

// HostedFunction 是投影给 hosted 包调用的宿主函数：JSON 请求进、JSON 响应出。
// guest 通过 //go:wasmimport <Module> <Name> 以 (reqPtr, reqLen, respPtr, respCap) -> respLen
// 的线性内存 ABI 调用；权限投影以宿主侧"当前调用上下文"为准，guest 无法伪造上下文。
type HostedFunction struct {
	Module string
	Name   string
	Call   func(context.Context, contracts.RequestContext, []byte) ([]byte, error)
}

// WasmHostConfig 装配 hosted 沙箱执行器。
type WasmHostConfig struct {
	// ReadArtifact 返回与 manifest 锁定的 .wasm 工件字节（负责 digest 与大小校验）。
	ReadArtifact func(context.Context, Manifest) ([]byte, error)
	// MemoryLimitPages 是 guest 线性内存上限（页，每页 64 KiB）；0 使用默认值。
	MemoryLimitPages uint32
	// MaxArtifactBytes 是工件字节上限；0 使用默认值。
	MaxArtifactBytes int64
	// CallTimeout 是单次 hosted 调用的执行时间预算；预算耗尽时强制终止 guest。
	// 0 使用默认值（30 秒）。没有"不限时"路径：每次调用必然带预算。
	CallTimeout time.Duration
	// HostFunctionsFor 按清单返回投影给该 guest 的宿主函数（可空：无宿主函数
	// 时声明了宿主函数依赖的清单在 Verify 期 fail-closed）。按清单提供使宿主
	// 函数可绑定到包自身的声明（如 ailuo.store 的 namespace 隔离）。
	HostFunctionsFor func(Manifest) ([]HostedFunction, error)
	// RequireHostFunctions 为 true 时只接受声明了宿主函数的清单（安装目录 hosted
	// 包专用宿主）：无宿主函数声明的包交给外部 GRPCHost 执行，避免双宿主歧义。
	RequireHostFunctions bool
}

// WasmHost 以 wazero 沙箱执行 hosted 包：进程内线性内存隔离 + WASI 能力裁剪。
// 每次调用创建独立实例，guest 之间与调用之间零共享状态。
type WasmHost struct {
	config WasmHostConfig
}

// NewWasmHost 构造 hosted 沙箱宿主；配置非法时返回显式错误。
func NewWasmHost(config WasmHostConfig) (*WasmHost, error) {
	if config.ReadArtifact == nil {
		return nil, ErrUnavailable
	}
	if config.MemoryLimitPages == 0 {
		config.MemoryLimitPages = hostedDefaultMemoryPages
	}
	if config.MaxArtifactBytes <= 0 {
		config.MaxArtifactBytes = hostedDefaultMaxArtifactBytes
	}
	if config.CallTimeout <= 0 {
		config.CallTimeout = hostedDefaultCallTimeout
	}
	return &WasmHost{config: config}, nil
}

// manifestHostFunctions 解析按清单提供的宿主函数并校验：函数字段完整、模块名
// 唯一。提供者返回的错误原样上浮（装配期的声明问题不得延迟到调用期）。
func (h *WasmHost) manifestHostFunctions(manifest Manifest) ([]HostedFunction, error) {
	if h.config.HostFunctionsFor == nil {
		if len(manifest.HostFunctions) > 0 {
			return nil, fmt.Errorf("%w: host provides no host functions for package %q",
				ErrInvalidManifest, manifest.ID)
		}
		return nil, nil
	}
	functions, err := h.config.HostFunctionsFor(manifest)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(functions))
	for _, fn := range functions {
		if fn.Module == "" || fn.Name == "" || fn.Call == nil {
			return nil, ErrUnavailable
		}
		key := packagecontract.HostedFunctionKey(fn.Module, fn.Name)
		if _, exists := seen[key]; exists {
			return nil, ErrDuplicateID
		}
		seen[key] = struct{}{}
	}
	return functions, nil
}

// Mode 返回宿主服务的运行模式：hosted 沙箱。
func (h *WasmHost) Mode() string { return ModeHosted }

// Verify 确认工件可读、未超过大小上限，且清单声明的宿主函数全部由本宿主
// 提供（声明 ⊆ 可用，缺项在注册期 fail-closed）；digest 校验由 ReadArtifact 负责。
// RequireHostFunctions 时无宿主函数声明的清单直接拒绝（本宿主只服务需内核
// 投影的安装目录 hosted 包，无声明者归 GRPCHost，防路由歧义）。
func (h *WasmHost) Verify(ctx context.Context, manifest Manifest) error {
	if h.config.RequireHostFunctions && len(manifest.HostFunctions) == 0 {
		return fmt.Errorf("%w: host requires host_functions declarations", ErrUnsupportedMode)
	}
	functions, err := h.manifestHostFunctions(manifest)
	if err != nil {
		return err
	}
	artifact, err := h.config.ReadArtifact(ctx, manifest)
	if err != nil {
		return err
	}
	if int64(len(artifact)) > h.config.MaxArtifactBytes {
		return ErrInvalidManifest
	}
	available := make(map[string]struct{}, len(functions))
	for _, fn := range functions {
		available[packagecontract.HostedFunctionKey(fn.Module, fn.Name)] = struct{}{}
	}
	for _, decl := range manifest.HostFunctions {
		if _, ok := available[packagecontract.HostedFunctionKey(decl.Module, decl.Name)]; !ok {
			return fmt.Errorf("%w: host function %s.%s is not provided by this host",
				ErrInvalidManifest, decl.Module, decl.Name)
		}
	}
	return nil
}

// Load 编译工件并装配宿主函数，返回沙箱 Runtime；编译失败快速失败。
// 加载期强制"只投影声明集合"：guest 只能 import 清单声明的宿主函数（WASI
// 除外），未声明导入直接拒绝，宿主函数实现仍只来自宿主配置。
func (h *WasmHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
	// 装配期问题（提供者错误、重复函数）先于编译快速失败，不产生半装配运行时。
	functions, err := h.manifestHostFunctions(manifest)
	if err != nil {
		return nil, err
	}
	artifact, err := h.config.ReadArtifact(ctx, manifest)
	if err != nil {
		return nil, err
	}
	if int64(len(artifact)) > h.config.MaxArtifactBytes {
		return nil, ErrInvalidManifest
	}
	wazeroRuntime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(h.config.MemoryLimitPages).
		WithCloseOnContextDone(true))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, wazeroRuntime); err != nil {
		_ = wazeroRuntime.Close(ctx)
		return nil, errors.Join(ErrLoadFailed, err)
	}
	compiled, err := wazeroRuntime.CompileModule(ctx, artifact)
	if err != nil {
		_ = wazeroRuntime.Close(ctx)
		return nil, errors.Join(ErrLoadFailed, err)
	}
	if err := checkDeclaredHostFunctionImports(compiled, manifest.HostFunctions); err != nil {
		_ = wazeroRuntime.Close(ctx)
		return nil, errors.Join(ErrLoadFailed, err)
	}
	projected := functions
	hosted := &wasmRuntime{
		wazeroRuntime: wazeroRuntime,
		compiled:      compiled,
		id:            manifest.ID,
		version:       manifest.Version,
		callTimeout:   h.config.CallTimeout,
		hostFuncs:     make(map[string]func(context.Context, contracts.RequestContext, []byte) ([]byte, error), len(projected)),
	}
	for _, fn := range projected {
		hosted.hostFuncs[packagecontract.HostedFunctionKey(fn.Module, fn.Name)] = fn.Call
	}
	if err := hosted.registerHostFunctions(ctx, projected); err != nil {
		_ = wazeroRuntime.Close(ctx)
		return nil, errors.Join(ErrLoadFailed, err)
	}
	return hosted, nil
}

// checkDeclaredHostFunctionImports 校验编译产物的函数导入：除 WASI 外，所有
// 导入必须属于清单声明的宿主函数集合，未声明导入在加载期拒绝（fail-closed）。
func checkDeclaredHostFunctionImports(compiled wazero.CompiledModule, decls []packagecontract.HostedFunctionDecl) error {
	declared := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		declared[packagecontract.HostedFunctionKey(decl.Module, decl.Name)] = struct{}{}
	}
	for _, definition := range compiled.ImportedFunctions() {
		moduleName, name, isImport := definition.Import()
		if !isImport || moduleName == wasiModuleName {
			continue
		}
		if _, ok := declared[packagecontract.HostedFunctionKey(moduleName, name)]; !ok {
			return fmt.Errorf("guest imports undeclared host function %s.%s", moduleName, name)
		}
	}
	return nil
}

// wasmRuntime 是 hosted 包的执行单元：共享 wazero 运行时与编译产物，每次调用独立实例化。
type wasmRuntime struct {
	wazeroRuntime wazero.Runtime
	compiled      wazero.CompiledModule
	id            string
	version       string
	callTimeout   time.Duration
	hostFuncs     map[string]func(context.Context, contracts.RequestContext, []byte) ([]byte, error)
	hostCalls     sync.Map // api.Module -> *hostedInvokeState

	stopOnce sync.Once
	stopErr  error
}

// hostedInvokeState 绑定一次调用的治理上下文，供宿主函数做权限投影与审计。
type hostedInvokeState struct {
	ctx     context.Context
	request contracts.RequestContext
	funcs   map[string]func(context.Context, contracts.RequestContext, []byte) ([]byte, error)
}

func (r *wasmRuntime) Describe(context.Context) (Description, error) {
	return Description{ID: r.id, Version: r.version, Mode: ModeHosted}, nil
}

func (r *wasmRuntime) Start(context.Context) error { return nil }

func (r *wasmRuntime) Health(context.Context) error { return nil }

// Stop 关闭共享 wazero 运行时；幂等，多次调用返回首次结果。
func (r *wasmRuntime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.stopErr = r.wazeroRuntime.Close(ctx)
	})
	return r.stopErr
}

// registerHostFunctions 把宿主函数投影为 guest 可 import 的 wasm 模块。
func (r *wasmRuntime) registerHostFunctions(ctx context.Context, functions []HostedFunction) error {
	byModule := make(map[string][]HostedFunction, len(functions))
	for _, fn := range functions {
		byModule[fn.Module] = append(byModule[fn.Module], fn)
	}
	for moduleName, moduleFunctions := range byModule {
		builder := r.wazeroRuntime.NewHostModuleBuilder(moduleName)
		for _, fn := range moduleFunctions {
			fn := fn
			builder = builder.NewFunctionBuilder().WithFunc(r.hostFunction(fn)).Export(fn.Name)
		}
		if _, err := builder.Instantiate(ctx); err != nil {
			return err
		}
	}
	return nil
}

// hostFunction 生成线性内存 ABI 的宿主函数：读请求、执行投影函数、写响应、返回长度。
// 调用方模块标识实例，从而取到该次调用的治理上下文。
func (r *wasmRuntime) hostFunction(fn HostedFunction) func(context.Context, api.Module, uint32, uint32, uint32, uint32) uint32 {
	return func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtr, responseCap uint32) uint32 {
		value, ok := r.hostCalls.Load(module)
		if !ok {
			return hostedHostFunctionError
		}
		state := value.(*hostedInvokeState)
		// 必须走 HostedFunctionKey：手写拼接与注册端不一致时查表恒失败。
		call, ok := state.funcs[packagecontract.HostedFunctionKey(fn.Module, fn.Name)]
		if !ok {
			return hostedHostFunctionError
		}
		request, ok := module.Memory().Read(requestPtr, requestLen)
		if !ok {
			return hostedHostFunctionError
		}
		response, err := call(state.ctx, state.request, request)
		if err != nil {
			return hostedHostFunctionError
		}
		if uint32(len(response)) > responseCap {
			return hostedHostFunctionError
		}
		if !module.Memory().Write(responsePtr, response) {
			return hostedHostFunctionError
		}
		return uint32(len(response))
	}
}

// hostedRequest 是宿主写入 guest stdin 的调用信封：Capability 标识 + 业务载荷。
// guest 据此分发到具体实现，并可用宿主函数读取治理上下文。
type hostedRequest struct {
	CapabilityID string          `json:"capability_id"`
	Payload      json.RawMessage `json:"payload"`
}

// Invoke 以独立实例执行一次调用：stdin 写入调用信封，stdout 读取结果信封。
// 先实例化（不自动执行入口）再登记治理上下文，随后手动调用 _start，避免
// 入口执行期间宿主函数查不到调用上下文。
func (r *wasmRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	requestEnvelope, err := json.Marshal(hostedRequest{CapabilityID: request.CapabilityID, Payload: payload})
	if err != nil {
		return nil, errors.Join(ErrRuntimeProtocol, err)
	}
	var stdout limitedBuffer
	config := wazero.NewModuleConfig().
		WithStdin(bytes.NewReader(requestEnvelope)).
		WithStdout(&stdout).
		WithStderr(io.Discard).
		WithSysWalltime().
		WithSysNanotime().
		WithStartFunctions()
	// 每次调用绑定执行时间预算：wazero 编译期周期检查（WithCloseOnContextDone）
	// 在预算耗尽时强制终止 guest 并关闭模块。不存在"不限时"路径。
	budgetContext, cancelBudget := context.WithTimeout(ctx, r.callTimeout)
	defer cancelBudget()
	module, err := r.wazeroRuntime.InstantiateModule(budgetContext, r.compiled, config)
	if err != nil {
		return nil, errors.Join(ErrRuntimeProtocol, err)
	}
	state := &hostedInvokeState{ctx: budgetContext, request: request, funcs: r.hostFuncs}
	r.hostCalls.Store(module, state)
	defer func() {
		r.hostCalls.Delete(module)
		_ = module.Close(ctx)
	}()

	start := module.ExportedFunction("_start")
	if start == nil {
		return nil, ErrRuntimeProtocol
	}
	if _, err := start.Call(budgetContext); err != nil {
		var exit *sys.ExitError
		switch {
		case errors.As(err, &exit) && exit.ExitCode() == 0:
			// Go 编译的 wasm 以 proc_exit(0) 正常结束。
		case errors.Is(err, context.DeadlineExceeded):
			// 预算耗尽被强制终止：归类为稳定超时，不视为协议违例。
			if ctx.Err() == nil {
				observe.Warn(ctx, "hosted 调用超过执行时间预算，已强制终止",
					observe.StringAttr("runtime_id", r.id),
					observe.Int64Attr("call_timeout_ms", r.callTimeout.Milliseconds()),
				)
			}
			return nil, err
		case errors.Is(err, context.Canceled):
			return nil, err
		default:
			// 其余退出码视为协议违例。
			return nil, errors.Join(ErrRuntimeProtocol, err)
		}
	}
	if stdout.overflowed {
		return nil, ErrRuntimeProtocol
	}
	return parseHostedEnvelope(r.id, stdout.Buffer())
}

// hostedEnvelope 是 hosted 包的统一结果信封：成功携带 result，失败携带闭式错误码。
type hostedEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// parseHostedEnvelope 解析结果信封：成功返回业务结果，失败按闭式错误码映射到
// 稳定内部错误（数据治理错误保留类别），guest 提供的消息只记入日志不外泄。
func parseHostedEnvelope(runtimeID string, output []byte) (json.RawMessage, error) {
	var envelope hostedEnvelope
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, errors.Join(ErrRuntimeProtocol, err)
	}
	if envelope.OK {
		return envelope.Result, nil
	}
	observe.Warn(context.Background(), "hosted 包拒绝了调用",
		observe.StringAttr("runtime_id", runtimeID),
		observe.StringAttr("hosted_error_code", envelope.Code),
		observe.StringAttr("reason", envelope.Message),
	)
	switch envelope.Code {
	case "data_unavailable", "data_incomplete", "data_expired":
		return nil, errors.Join(ErrHostedCallRejected, InvocationError{Code: envelope.Code})
	case "data_untrusted":
		return nil, errors.Join(ErrHostedCallRejected, InvocationError{Code: "data_non_authoritative"})
	case "invalid_argument":
		return nil, errors.Join(ErrHostedCallRejected, InvocationError{Code: "invalid_arguments"})
	case "internal":
		return nil, ErrHostedCallRejected
	default:
		// 未知错误码视为协议违例：不允许 guest 自定义错误码进入内核错误面。
		return nil, ErrRuntimeProtocol
	}
}

// limitedBuffer 限制 guest 输出字节数：超限标记 overflowed 并停止接收。
type limitedBuffer struct {
	buffer     bytes.Buffer
	overflowed bool
}

// Write 在超过 hostedStdoutLimit 后停止接收并标记溢出。
func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := hostedStdoutLimit - b.buffer.Len()
	if remaining <= 0 {
		b.overflowed = true
		return len(p), io.ErrShortWrite
	}
	if len(p) > remaining {
		b.overflowed = true
		written, _ := b.buffer.Write(p[:remaining])
		return written, io.ErrShortWrite
	}
	return b.buffer.Write(p)
}

// Buffer 返回已接收的字节。
func (b *limitedBuffer) Buffer() []byte {
	return b.buffer.Bytes()
}
