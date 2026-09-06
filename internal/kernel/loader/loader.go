package loader

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
)

const (
	ModeHosted   = "hosted"
	ModeIsolated = "isolated"

	// RoleCapability 是能力提供者角色：经 Dispatcher 被内核调用（Invoke），
	// 实现 Invoker 接口；具体包不属于 Loader 的固定知识。
	RoleCapability = "capability"
	// RoleExecutor 是 AI 执行者角色：驱动受治理 Run 会话、反向消费内核投影的
	// 能力，实现 internal/kernel/executor 契约，不注册任何被调能力。
	RoleExecutor = "executor"

	StateRegistered = "registered"
	StateLoading    = "loading"
	StateReady      = "ready"
	StateUnloading  = "unloading"
	StateFailed     = "failed"
)

var (
	ErrInvalidManifest  = errors.New("invalid runtime manifest")
	ErrDuplicateID      = errors.New("runtime is already registered")
	ErrNotFound         = errors.New("runtime is not registered")
	ErrUnsupportedMode  = errors.New("runtime mode is unsupported")
	ErrLoadFailed       = errors.New("runtime load failed")
	ErrUnavailable      = errors.New("runtime is unavailable")
	ErrInFlight         = errors.New("runtime has in-flight calls")
	ErrShuttingDown     = errors.New("runtime loader is shutting down")
	ErrDescribeMismatch = errors.New("runtime description does not match lock")
	ErrRuntimeBusy      = errors.New("runtime host capacity is exhausted")
)

var stableIDPattern = id.AppID

// Manifest 是运行时的注册与装载清单。ID/Version/Mode 是身份字段；Role、
// Pin、IdleTTL、LockedDigest 与 HostFunctions 是声明字段——装载与绑定校验
// 以完整清单为准，工件读取只依赖身份字段。
type Manifest struct {
	ID           string
	Version      string
	Mode         string
	Role         string
	LockedDigest string
	Pin          bool
	IdleTTL      time.Duration
	// HostFunctions 是包声明的宿主函数依赖（仅 hosted 有意义）：guest 只可
	// 调用清单声明且宿主提供的宿主函数，未声明调用在加载期被拒绝。
	HostFunctions []packagecontract.HostedFunctionDecl
	// Storage 是包声明的持久化契约（namespace 等）。声明了存储宿主函数的包
	// 必须同时声明该段；存储函数的 namespace 绑定取自此处，guest 不可选择。
	Storage *packagecontract.Storage
}

// Equal 比较运行时清单的完整身份与装载声明。
func (m Manifest) Equal(other Manifest) bool {
	return m.ID == other.ID && m.Version == other.Version && m.Mode == other.Mode &&
		m.Role == other.Role && m.LockedDigest == other.LockedDigest && m.Pin == other.Pin &&
		m.IdleTTL == other.IdleTTL && packagecontract.EqualHostedFunctions(m.HostFunctions, other.HostFunctions) &&
		packagecontract.EqualStorage(m.Storage, other.Storage)
}

// SameIdentity 只比较装载工件所需的运行时身份字段。
func (m Manifest) SameIdentity(other Manifest) bool {
	return m.ID == other.ID && m.Version == other.Version && m.Mode == other.Mode
}

type Description struct {
	ID      string
	Version string
	Mode    string
}

// Runtime 是已加载运行时的生命周期面，全部角色共有。能力面按角色拆分：
// 能力提供者实现 Invoker（被 Dispatcher 调用），AI 执行者实现
// internal/kernel/executor.ClientProvider（驱动 Run 会话），角色由清单声明、
// 加载期校验，不存在"不适用"的运行时方法。
type Runtime interface {
	Describe(context.Context) (Description, error)
	Start(context.Context) error
	Health(context.Context) error
	Stop(context.Context) error
}

// Invoker 是能力提供者角色的执行面：以治理上下文执行一次调用。
type Invoker interface {
	Invoke(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
}

// Host 只从已经安装并锁定的本地单元加载实现；校验失败时不得执行 Load。
// Mode 声明宿主服务的运行模式；一个宿主只服务一种模式，同一模式允许多个宿主
// （如进程内 WasmHost 与外部 hosted 的 GRPCHost），
// 清单在注册时按 Verify 精确绑定到唯一宿主。
type Host interface {
	Mode() string
	Verify(context.Context, Manifest) error
	Load(context.Context, Manifest) (Runtime, error)
}

type HostCloser interface {
	Close(context.Context) error
}

type entry struct {
	manifest Manifest
	// host 是注册时按 Verify 精确绑定、能加载该清单的宿主；同一模式存在多个
	// 宿主时，绑定结果在注册期一次性确定并固化，加载期不再重新选择。
	// Upgrade 切换版本时随候选重新绑定。
	host Host

	mu              sync.Mutex
	state           string
	runtime         Runtime
	inFlight        int
	currentInFlight int
	transition      chan struct{}
	// retired 是升级后被替换、仍在 drain 的旧版本运行时；在途调用排空后停止。
	retired []*retiredRuntime
}

// retiredRuntime 是被升级替换的旧版本运行时：inFlight 是其剩余在途调用数，
// 归零后经 stopOnce 恰好停止一次；stopped 通道供 Shutdown 有界等待。
// group 不为 nil 时表示该成员属于退役组，停止由组统一触发。
type retiredRuntime struct {
	runtime  Runtime
	inFlight int
	stopOnce sync.Once
	stopped  chan struct{}
	group    *retiredGroup
}

type Manager struct {
	mu             sync.RWMutex
	entries        map[string]*entry
	hosts          map[string][]Host
	packages       map[string]*packageGroup
	accepting      bool
	activeUpgrades int
	upgradeDone    chan struct{}
	now            func() time.Time
}

// packageGroup 是一个包的组件组：order 按依赖拓扑排列（Provider 在前）。
type packageGroup struct {
	order     []*entry
	upgradeMu sync.Mutex
}

// New 构造统一 Loader：一个 Manager 持有全部运行模式的宿主，模式内部允许
// 多个宿主（不同包共享同一 Loader，不按包分叉 Manager）。
func New(hosts ...Host) (*Manager, error) {
	if len(hosts) == 0 {
		return nil, ErrUnavailable
	}
	grouped := make(map[string][]Host)
	for _, host := range hosts {
		if host == nil {
			return nil, ErrUnavailable
		}
		mode := host.Mode()
		if mode != ModeHosted && mode != ModeIsolated {
			return nil, ErrUnsupportedMode
		}
		grouped[mode] = append(grouped[mode], host)
	}
	upgradeDone := make(chan struct{})
	close(upgradeDone)
	return &Manager{
		entries:     make(map[string]*entry),
		hosts:       grouped,
		packages:    make(map[string]*packageGroup),
		accepting:   true,
		upgradeDone: upgradeDone,
		now:         time.Now,
	}, nil
}

func (m *Manager) beginUpgrade() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return ErrShuttingDown
	}
	if m.activeUpgrades == 0 {
		m.upgradeDone = make(chan struct{})
	}
	m.activeUpgrades++
	return nil
}

func (m *Manager) endUpgrade() {
	m.mu.Lock()
	m.activeUpgrades--
	if m.activeUpgrades == 0 {
		close(m.upgradeDone)
	}
	m.mu.Unlock()
}

// selectHost 按清单 Verify 绑定唯一宿主：同一模式存在多个宿主时，恰好一个
// 宿主通过 Verify 才接受注册；零匹配（本 Deployment 无法承载）或歧义匹配
// （≥2）都在注册期 fail-closed，避免加载期出现不确定路由。
func (m *Manager) selectHost(ctx context.Context, manifest Manifest) (Host, error) {
	candidates := m.hosts[manifest.Mode]
	if len(candidates) == 0 {
		return nil, ErrUnsupportedMode
	}
	var matched Host
	matches := 0
	for _, host := range candidates {
		if err := host.Verify(ctx, manifest); err == nil {
			matched = host
			matches++
		}
	}
	switch matches {
	case 1:
		return matched, nil
	case 0:
		return nil, fmt.Errorf("%w: no host serves runtime manifest %q", ErrUnsupportedMode, manifest.ID)
	default:
		return nil, fmt.Errorf("%w: %d hosts serve runtime manifest %q", ErrInvalidManifest, matches, manifest.ID)
	}
}

// Pinned 返回全部已注册且清单声明 Pin=true 的运行时标识（排序），供启动预热
// 使用。pin 由各清单声明，内核装配不再按包
// 硬编码预热清单。
func (m *Manager) Pinned() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.entries))
	for id, item := range m.entries {
		if item.manifest.Pin {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Register 注册单个运行时清单，按 Verify 绑定唯一宿主。
func (m *Manager) Register(ctx context.Context, manifest Manifest) error {
	return m.RegisterBatch(ctx, []Manifest{manifest})
}

// RegisterBatch 在发布任何运行时前完成整批校验与宿主绑定，避免启动恢复留下部分目录。
func (m *Manager) RegisterBatch(ctx context.Context, manifests []Manifest) error {
	if len(manifests) == 0 || len(manifests) > 4096 {
		return ErrInvalidManifest
	}
	bound := make(map[string]Host, len(manifests))
	seen := make(map[string]struct{}, len(manifests))
	for _, manifest := range manifests {
		if err := ValidateManifest(manifest); err != nil {
			return err
		}
		host, err := m.selectHost(ctx, manifest)
		if err != nil {
			return err
		}
		if _, exists := seen[manifest.ID]; exists {
			return ErrDuplicateID
		}
		seen[manifest.ID] = struct{}{}
		bound[manifest.ID] = host
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return ErrShuttingDown
	}
	for _, manifest := range manifests {
		if _, exists := m.entries[manifest.ID]; exists {
			return ErrDuplicateID
		}
	}
	for _, manifest := range manifests {
		m.entries[manifest.ID] = &entry{
			manifest: manifest,
			host:     bound[manifest.ID],
			state:    StateRegistered,
		}
	}
	return nil
}

// stopRetiredRuntime 恰好停止一次旧版本运行时，并在停止后关闭 stopped 通道。
func stopRetiredRuntime(retired *retiredRuntime) {
	retired.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		defer cancel()
		stopErr := retired.runtime.Stop(ctx)
		close(retired.stopped)
		if stopErr != nil {
			observe.Error(ctx, "升级后旧版本运行时停止失败", stopErr)
		}
	})
}

func (m *Manager) rollbackRegistered(manifests []Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, manifest := range manifests {
		item := m.entries[manifest.ID]
		if item == nil {
			continue
		}
		item.mu.Lock()
		safe := item.state == StateRegistered && item.runtime == nil && item.inFlight == 0 &&
			item.manifest.Equal(manifest)
		item.mu.Unlock()
		if !safe {
			return ErrUnavailable
		}
	}
	for _, manifest := range manifests {
		delete(m.entries, manifest.ID)
	}
	return nil
}

func (m *Manager) EnsureLoaded(ctx context.Context, id string) error {
	item, err := m.resolve(id)
	if err != nil {
		return err
	}
	return m.ensureLoaded(ctx, item)
}

func (m *Manager) ensureLoaded(ctx context.Context, item *entry) error {
	for {
		item.mu.Lock()
		switch item.state {
		case StateReady:
			item.mu.Unlock()
			return nil
		case StateFailed:
			item.mu.Unlock()
			return ErrLoadFailed
		case StateLoading, StateUnloading:
			wait := item.transition
			item.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
				continue
			}
		case StateRegistered:
			item.state = StateLoading
			item.transition = make(chan struct{})
			wait := item.transition
			item.mu.Unlock()
			started := m.now()
			loadErr := loadRuntime(ctx, item.host, item.manifest)
			item.mu.Lock()
			if loadErr.err != nil {
				item.state = StateFailed
				item.runtime = loadErr.runtime
			} else {
				item.state = StateReady
				item.runtime = loadErr.runtime
			}
			close(wait)
			item.transition = nil
			item.mu.Unlock()
			observe.DefaultMetrics().ObserveRuntimeLoad(loadErr.err == nil, m.now().Sub(started))
			if loadErr.err != nil {
				observe.Error(ctx, "运行时实现加载失败", loadErr.err,
					observe.StringAttr("runtime_id", item.manifest.ID),
					observe.StringAttr("runtime_version", item.manifest.Version),
					observe.StringAttr("runtime_mode", item.manifest.Mode),
					observe.Duration(started),
				)
				return errors.Join(ErrLoadFailed, loadErr.err)
			}
			observe.Info(ctx, "运行时实现已加载并通过健康检查",
				observe.StringAttr("runtime_id", item.manifest.ID),
				observe.StringAttr("runtime_version", item.manifest.Version),
				observe.StringAttr("runtime_mode", item.manifest.Mode),
				observe.Duration(started),
			)
			return nil
		default:
			item.mu.Unlock()
			return ErrUnavailable
		}
	}
}

type loadResult struct {
	runtime Runtime
	err     error
}

func loadRuntime(ctx context.Context, host Host, manifest Manifest) loadResult {
	if err := host.Verify(ctx, manifest); err != nil {
		return loadResult{err: fmt.Errorf("verify installed runtime: %w", err)}
	}
	runtime, err := host.Load(ctx, manifest)
	if err != nil || runtime == nil {
		return loadResult{err: errors.Join(ErrUnavailable, err)}
	}
	description, err := runtime.Describe(ctx)
	if err != nil {
		return stopAfterLoadFailure(ctx, runtime, err)
	}
	if description.ID != manifest.ID || description.Version != manifest.Version || description.Mode != manifest.Mode {
		return stopAfterLoadFailure(ctx, runtime, ErrDescribeMismatch)
	}
	// 角色与执行面一致性在加载期强制：能力提供者必须实现 Invoker；执行者
	// 必须实现 internal/kernel/executor 契约（ClientProvider）。
	if manifest.Role == RoleCapability {
		if _, ok := runtime.(Invoker); !ok {
			return stopAfterLoadFailure(ctx, runtime, ErrInvalidManifest)
		}
	} else {
		if _, ok := runtime.(executor.ClientProvider); !ok {
			return stopAfterLoadFailure(ctx, runtime, ErrInvalidManifest)
		}
	}
	if err := runtime.Start(ctx); err != nil {
		return stopAfterLoadFailure(ctx, runtime, err)
	}
	if err := runtime.Health(ctx); err != nil {
		return stopAfterLoadFailure(ctx, runtime, err)
	}
	return loadResult{runtime: runtime}
}

func stopAfterLoadFailure(ctx context.Context, runtime Runtime, primary error) loadResult {
	stopContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stopErr := runtime.Stop(stopContext)
	if stopErr != nil {
		return loadResult{runtime: runtime, err: errors.Join(primary, stopErr)}
	}
	return loadResult{err: primary}
}

type Lease struct {
	entry   *entry
	runtime Runtime
	invoker Invoker
	once    sync.Once
}

func (m *Manager) Acquire(ctx context.Context, id string) (*Lease, error) {
	m.mu.RLock()
	accepting := m.accepting
	m.mu.RUnlock()
	if !accepting {
		return nil, ErrShuttingDown
	}
	if err := m.EnsureLoaded(ctx, id); err != nil {
		return nil, err
	}
	m.mu.RLock()
	accepting = m.accepting
	if !accepting {
		m.mu.RUnlock()
		return nil, ErrShuttingDown
	}
	item := m.entries[id]
	if item == nil {
		m.mu.RUnlock()
		return nil, ErrNotFound
	}
	item.mu.Lock()
	if item.state != StateReady || item.runtime == nil {
		item.mu.Unlock()
		m.mu.RUnlock()
		return nil, ErrUnavailable
	}
	var invoker Invoker
	if item.manifest.Role == RoleCapability {
		// 能力提供者的 Invoker 由加载期校验保证；取不到视为内部违例。
		var ok bool
		invoker, ok = item.runtime.(Invoker)
		if !ok {
			item.mu.Unlock()
			m.mu.RUnlock()
			return nil, ErrUnavailable
		}
	}
	item.inFlight++
	item.currentInFlight++
	loadedRuntime := item.runtime
	item.mu.Unlock()
	m.mu.RUnlock()
	observe.DefaultMetrics().RuntimeCallStarted()
	return &Lease{entry: item, runtime: loadedRuntime, invoker: invoker}, nil
}

func (l *Lease) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if l == nil || l.runtime == nil || l.invoker == nil {
		return nil, ErrUnavailable
	}
	result, err := l.invoker.Invoke(ctx, request, payload)
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRuntimeProtocol) {
		l.entry.mu.Lock()
		if l.entry.state == StateReady && l.entry.runtime == l.runtime {
			l.entry.state = StateFailed
		}
		l.entry.mu.Unlock()
	}
	return result, err
}

// Runtime 返回租约持有的运行时（供调用方获取协议级客户端等）。
func (l *Lease) Runtime() Runtime {
	if l == nil {
		return nil
	}
	return l.runtime
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		item := l.entry
		item.mu.Lock()
		item.inFlight--
		var retired *retiredRuntime
		if l.runtime == item.runtime {
			item.currentInFlight--
		} else {
			for _, candidate := range item.retired {
				if candidate.runtime == l.runtime {
					retired = candidate
					break
				}
			}
			if retired != nil {
				retired.inFlight--
			}
		}
		drained := retired != nil && retired.inFlight <= 0
		item.mu.Unlock()
		observe.DefaultMetrics().RuntimeCallStopped()
		if drained {
			// 升级替换的旧版本在途调用已排空：组员交给退役组协调（反序停止），
			// 独立成员直接停止（恰好一次）。
			if retired.group != nil {
				retired.group.memberDrained()
			} else {
				stopRetiredRuntime(retired)
			}
		}
	})
}

// ID 返回租约持有运行时的清单标识。
func (l *Lease) ID() string {
	if l == nil || l.entry == nil {
		return ""
	}
	return l.entry.manifest.ID
}

// Handler 返回能力提供者角色的 Dispatcher 路由入口；执行者角色不注册任何
// 被调能力，构造路由时直接拒绝（角色在注册期已校验，此检查是防御性边界）。
func (m *Manager) Handler(id string) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		lease, err := m.Acquire(ctx, id)
		if err != nil {
			return nil, err
		}
		defer lease.Release()
		return lease.Invoke(ctx, request, payload)
	}
}

// Executor 解析本 Deployment 唯一的执行者运行时（清单声明 RoleExecutor），
// 返回其租约；零个或多个执行者都 fail-closed，避免装配期不确定路由。调用方
// 负责 Release，并按 internal/kernel/executor 契约取用客户端。
func (m *Manager) Executor(ctx context.Context) (*Lease, error) {
	m.mu.RLock()
	var id string
	matches := 0
	for _, item := range m.entries {
		if item.manifest.Role == RoleExecutor {
			matches++
			id = item.manifest.ID
		}
	}
	m.mu.RUnlock()
	switch matches {
	case 1:
		return m.Acquire(ctx, id)
	case 0:
		return nil, fmt.Errorf("%w: no executor runtime is registered", ErrNotFound)
	default:
		return nil, fmt.Errorf("%w: %d executor runtimes are registered, expected exactly one", ErrInvalidManifest, matches)
	}
}

func (m *Manager) Warmup(ctx context.Context, ids []string, concurrency int) error {
	if concurrency < 1 || concurrency > 64 {
		return ErrInvalidManifest
	}
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !stableIDPattern.MatchString(id) {
			return ErrNotFound
		}
		unique[id] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for id := range unique {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	warmupGroups := make([][]string, 0, len(ordered))
	covered := make(map[string]struct{}, len(ordered))
	m.mu.RLock()
	packageIDs := make([]string, 0, len(m.packages))
	for packageID := range m.packages {
		packageIDs = append(packageIDs, packageID)
	}
	sort.Strings(packageIDs)
	for _, packageID := range packageIDs {
		group := m.packages[packageID]
		idsInOrder := make([]string, 0, len(group.order))
		for _, item := range group.order {
			id := item.manifest.ID
			if _, ok := unique[id]; !ok {
				continue
			}
			idsInOrder = append(idsInOrder, id)
			covered[id] = struct{}{}
		}
		if len(idsInOrder) > 0 {
			warmupGroups = append(warmupGroups, idsInOrder)
		}
	}
	m.mu.RUnlock()
	for _, id := range ordered {
		if _, ok := covered[id]; !ok {
			warmupGroups = append(warmupGroups, []string{id})
		}
	}
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan []string)
	failures := make(chan error, len(warmupGroups))
	var workers sync.WaitGroup
	for index := 0; index < concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				for _, id := range group {
					if err := m.EnsureLoaded(workerContext, id); err != nil {
						failures <- fmt.Errorf("warm runtime %s: %w", id, err)
						cancel()
						return
					}
				}
			}
		}()
	}
sendJobs:
	for _, group := range warmupGroups {
		select {
		case jobs <- group:
		case <-workerContext.Done():
			break sendJobs
		}
	}
	close(jobs)
	workers.Wait()
	close(failures)
	var result []error
	for err := range failures {
		result = append(result, err)
	}
	return errors.Join(result...)
}

// RecoverFailed 在所有在途调用结束后清理失败句柄，使下一次调用重新执行完整加载流程。
func (m *Manager) RecoverFailed(ctx context.Context, id string) error {
	item, err := m.resolve(id)
	if err != nil {
		return err
	}
	item.mu.Lock()
	failed := item.state == StateFailed
	hasRuntime := item.runtime != nil
	if failed && !hasRuntime {
		item.state = StateRegistered
	}
	item.mu.Unlock()
	if !failed {
		return ErrUnavailable
	}
	if !hasRuntime {
		return nil
	}
	return m.unload(ctx, item)
}

func (m *Manager) unload(ctx context.Context, item *entry) error {
	item.mu.Lock()
	if item.inFlight > 0 {
		item.mu.Unlock()
		return ErrInFlight
	}
	if item.state == StateRegistered {
		item.mu.Unlock()
		return nil
	}
	if item.runtime == nil || (item.state != StateReady && item.state != StateFailed) {
		item.mu.Unlock()
		return ErrUnavailable
	}
	runtime := item.runtime
	item.state = StateUnloading
	item.transition = make(chan struct{})
	wait := item.transition
	item.mu.Unlock()

	started := m.now()
	stopErr := runtime.Stop(ctx)
	item.mu.Lock()
	if stopErr != nil {
		item.state = StateFailed
	} else {
		item.state = StateRegistered
		item.runtime = nil
	}
	close(wait)
	item.transition = nil
	item.mu.Unlock()
	observe.DefaultMetrics().ObserveRuntimeStop(stopErr == nil, m.now().Sub(started))
	if stopErr != nil {
		observe.Error(ctx, "运行时实现停止失败", stopErr,
			observe.StringAttr("runtime_id", item.manifest.ID),
			observe.StringAttr("runtime_version", item.manifest.Version),
			observe.StringAttr("runtime_mode", item.manifest.Mode),
			observe.Duration(started),
		)
	} else {
		observe.Info(ctx, "运行时实现已经停止",
			observe.StringAttr("runtime_id", item.manifest.ID),
			observe.StringAttr("runtime_version", item.manifest.Version),
			observe.StringAttr("runtime_mode", item.manifest.Mode),
			observe.Duration(started),
		)
	}
	return stopErr
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.accepting = false
	upgradeDone := m.upgradeDone
	items := make([]*entry, 0, len(m.entries))
	for _, item := range m.entries {
		items = append(items, item)
	}
	m.mu.Unlock()
	select {
	case <-upgradeDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		allDrained := true
		for _, item := range items {
			item.mu.Lock()
			allDrained = allDrained && item.inFlight == 0 && item.state != StateLoading && item.state != StateUnloading
			item.mu.Unlock()
		}
		if allDrained {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	var result []error
	for _, item := range items {
		item.mu.Lock()
		ready := item.state == StateReady || (item.state == StateFailed && item.runtime != nil)
		retired := append([]*retiredRuntime(nil), item.retired...)
		item.mu.Unlock()
		if ready {
			if err := m.unload(ctx, item); err != nil {
				result = append(result, err)
			}
		}
		// 升级替换的旧版本在 inFlight 归零后已触发停止；此处兜底再次触发
		// 并等待其完成，避免在途全部释放前 Shutdown 提前返回。
		for _, old := range retired {
			item.mu.Lock()
			drained := old.inFlight <= 0
			item.mu.Unlock()
			if drained {
				stopRetiredRuntime(old)
			}
		}
	}
	// 有界等待升级替换的旧版本停止完成。
	for _, item := range items {
		item.mu.Lock()
		retired := append([]*retiredRuntime(nil), item.retired...)
		item.mu.Unlock()
		for _, old := range retired {
			select {
			case <-old.stopped:
			case <-ctx.Done():
				return errors.Join(append(result, ctx.Err())...)
			}
		}
	}
	for _, hosts := range m.hosts {
		for _, host := range hosts {
			if closer, ok := host.(HostCloser); ok {
				if err := closer.Close(ctx); err != nil {
					result = append(result, err)
				}
			}
		}
	}
	return errors.Join(result...)
}

func (m *Manager) resolve(id string) (*entry, error) {
	m.mu.RLock()
	item := m.entries[id]
	m.mu.RUnlock()
	if item == nil {
		return nil, ErrNotFound
	}
	return item, nil
}

func ValidateManifest(manifest Manifest) error {
	if !stableIDPattern.MatchString(manifest.ID) ||
		(manifest.Mode != ModeHosted && manifest.Mode != ModeIsolated) ||
		(manifest.Role != RoleCapability && manifest.Role != RoleExecutor) ||
		manifest.IdleTTL < 0 || len(manifest.LockedDigest) != 64 {
		return ErrInvalidManifest
	}
	if _, err := packagecontract.ParseVersion(manifest.Version); err != nil {
		return ErrInvalidManifest
	}
	if err := packagecontract.ValidateHostedFunctions(manifest.HostFunctions); err != nil {
		return ErrInvalidManifest
	}
	if manifest.Storage != nil {
		if err := packagecontract.ValidateStorage(*manifest.Storage); err != nil {
			return ErrInvalidManifest
		}
	}
	digest, err := hex.DecodeString(manifest.LockedDigest)
	if err != nil || len(digest) != 32 {
		return ErrInvalidManifest
	}
	return nil
}
