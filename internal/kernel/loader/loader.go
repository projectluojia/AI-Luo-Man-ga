package loader

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	ModeEmbedded = "embedded"
	ModeHosted   = "hosted"
	ModeIsolated = "isolated"

	StateRegistered = "registered"
	StateLoading    = "loading"
	StateReady      = "ready"
	StateUnloading  = "unloading"
	StateFailed     = "failed"
)

var (
	ErrInvalidManifest   = errors.New("invalid runtime manifest")
	ErrDuplicateID       = errors.New("runtime is already registered")
	ErrNotFound          = errors.New("runtime is not registered")
	ErrUnsupportedMode   = errors.New("runtime mode is unsupported")
	ErrLoadFailed        = errors.New("runtime load failed")
	ErrUnavailable       = errors.New("runtime is unavailable")
	ErrInFlight          = errors.New("runtime has in-flight calls")
	ErrPinned            = errors.New("runtime is pinned")
	ErrShuttingDown      = errors.New("runtime loader is shutting down")
	ErrDescribeMismatch  = errors.New("runtime description does not match lock")
	ErrCleanupRequired   = errors.New("failed runtime requires cleanup")
	ErrConcurrentUpgrade = errors.New("runtime was concurrently upgraded")
	ErrRuntimeBusy       = errors.New("runtime host capacity is exhausted")
)

var (
	stableIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	versionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

type Manifest struct {
	ID           string
	Version      string
	Mode         string
	LockedDigest string
	Pin          bool
	IdleTTL      time.Duration
}

type Description struct {
	ID      string
	Version string
	Mode    string
}

type Runtime interface {
	Describe(context.Context) (Description, error)
	Start(context.Context) error
	Health(context.Context) error
	Invoke(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
	Stop(context.Context) error
}

// Host 只从已经安装并锁定的本地单元加载实现；校验失败时不得执行 Load。
// Mode 声明宿主服务的运行模式；一个宿主只服务一种模式，同一模式允许多个宿主
// （如内置 campus 的进程内 WasmHost 与 installed hosted 的 GRPCHost），
// 清单在注册时按 Verify 精确绑定到唯一宿主。
type Host interface {
	Mode() string
	Verify(context.Context, Manifest) error
	Load(context.Context, Manifest) (Runtime, error)
}

type HostCloser interface {
	Close(context.Context) error
}

type Snapshot struct {
	ID         string
	Version    string
	Mode       string
	State      string
	Pin        bool
	InFlight   int
	LastUsedAt time.Time
}

type entry struct {
	manifest Manifest
	// host 是注册时按 Verify 精确绑定、能加载该清单的宿主；同一模式存在多个
	// 宿主时，绑定结果在注册期一次性确定并固化，加载期不再重新选择。
	host Host

	mu         sync.Mutex
	state      string
	runtime    Runtime
	inFlight   int
	lastUsedAt time.Time
	transition chan struct{}
}

type Manager struct {
	mu        sync.RWMutex
	entries   map[string]*entry
	retired   []*entry
	hosts     map[string][]Host
	accepting bool
	now       func() time.Time
}

// New 构造统一 Loader：一个 Manager 持有全部运行模式的宿主，模式内部允许
// 多个宿主（内置包与 installed 包共享同一 Loader，不再按包分叉多个 Manager）。
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
		if mode != ModeEmbedded && mode != ModeHosted && mode != ModeIsolated {
			return nil, ErrUnsupportedMode
		}
		grouped[mode] = append(grouped[mode], host)
	}
	return &Manager{
		entries:   make(map[string]*entry),
		hosts:     grouped,
		accepting: true,
		now:       time.Now,
	}, nil
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
		if err := validateManifest(manifest); err != nil {
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
			sameRuntimeManifest(item.manifest, manifest)
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
				item.lastUsedAt = m.now().UTC()
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

// Upgrade 先完整校验并启动新版本，再原子切换新流量，最后等待旧版本在途调用排空。
func (m *Manager) Upgrade(ctx context.Context, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	m.mu.RLock()
	accepting := m.accepting
	current := m.entries[manifest.ID]
	m.mu.RUnlock()
	if !accepting {
		return ErrShuttingDown
	}
	if current == nil {
		return ErrNotFound
	}
	if current.manifest.Version == manifest.Version && current.manifest.LockedDigest == manifest.LockedDigest {
		return ErrDuplicateID
	}
	host, err := m.selectHost(ctx, manifest)
	if err != nil {
		return err
	}
	candidate := &entry{manifest: manifest, host: host, state: StateRegistered}
	if err := m.ensureLoaded(ctx, candidate); err != nil {
		return err
	}

	m.mu.Lock()
	if !m.accepting {
		m.mu.Unlock()
		cleanupErr := m.cleanupRuntime(ctx, candidate)
		return errors.Join(ErrShuttingDown, cleanupErr)
	}
	if m.entries[manifest.ID] != current {
		m.mu.Unlock()
		cleanupErr := m.cleanupRuntime(ctx, candidate)
		return errors.Join(ErrConcurrentUpgrade, cleanupErr)
	}
	m.entries[manifest.ID] = candidate
	m.retired = append(m.retired, current)
	m.mu.Unlock()
	observe.Info(ctx, "运行时新版本已接管新调用",
		observe.StringAttr("runtime_id", manifest.ID),
		observe.StringAttr("runtime_version", manifest.Version),
		observe.StringAttr("runtime_mode", manifest.Mode),
	)

	if err := waitDrained(ctx, current); err != nil {
		return err
	}
	if err := m.unload(ctx, current, true); err != nil {
		return err
	}
	m.removeRetired(current)
	return nil
}

func (m *Manager) cleanupRuntime(ctx context.Context, item *entry) error {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return m.unload(cleanupContext, item, true)
}

func waitDrained(ctx context.Context, item *entry) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		item.mu.Lock()
		drained := item.inFlight == 0 && item.state != StateLoading && item.state != StateUnloading
		item.mu.Unlock()
		if drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrInFlight, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) removeRetired(target *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for index, item := range m.retired {
		if item == target {
			m.retired = append(m.retired[:index], m.retired[index+1:]...)
			return
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
	now     func() time.Time
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
	item.inFlight++
	item.lastUsedAt = m.now().UTC()
	loadedRuntime := item.runtime
	item.mu.Unlock()
	m.mu.RUnlock()
	observe.DefaultMetrics().RuntimeCallStarted()
	return &Lease{entry: item, runtime: loadedRuntime, now: m.now}, nil
}

func (l *Lease) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if l == nil || l.runtime == nil {
		return nil, ErrUnavailable
	}
	result, err := l.runtime.Invoke(ctx, request, payload)
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
		l.entry.mu.Lock()
		l.entry.inFlight--
		l.entry.lastUsedAt = l.now().UTC()
		l.entry.mu.Unlock()
		observe.DefaultMetrics().RuntimeCallStopped()
	})
}

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
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	failures := make(chan error, len(ordered))
	var workers sync.WaitGroup
	for index := 0; index < concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				if err := m.EnsureLoaded(workerContext, id); err != nil {
					failures <- fmt.Errorf("warm runtime %s: %w", id, err)
					cancel()
					return
				}
			}
		}()
	}
sendJobs:
	for _, id := range ordered {
		select {
		case jobs <- id:
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

func (m *Manager) Unload(ctx context.Context, id string) error {
	item, err := m.resolve(id)
	if err != nil {
		return err
	}
	return m.unload(ctx, item, false)
}

func (m *Manager) ResetFailed(id string) error {
	item, err := m.resolve(id)
	if err != nil {
		return err
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.state != StateFailed {
		return ErrUnavailable
	}
	if item.runtime != nil {
		return ErrCleanupRequired
	}
	item.state = StateRegistered
	return nil
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
	return m.unload(ctx, item, true)
}

func (m *Manager) unload(ctx context.Context, item *entry, force bool) error {
	item.mu.Lock()
	if item.manifest.Pin && !force {
		item.mu.Unlock()
		return ErrPinned
	}
	if item.inFlight > 0 {
		item.mu.Unlock()
		return ErrInFlight
	}
	if item.state == StateRegistered {
		item.mu.Unlock()
		return nil
	}
	if item.runtime == nil || (item.state != StateReady && !(force && item.state == StateFailed)) {
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

func (m *Manager) SweepIdle(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return ErrInvalidManifest
	}
	m.mu.RLock()
	items := make([]*entry, 0, len(m.entries))
	for _, item := range m.entries {
		items = append(items, item)
	}
	m.mu.RUnlock()
	var result []error
	for _, item := range items {
		item.mu.Lock()
		idle := !item.manifest.Pin && item.manifest.IdleTTL > 0 && item.state == StateReady &&
			item.inFlight == 0 && !now.Before(item.lastUsedAt.Add(item.manifest.IdleTTL))
		item.mu.Unlock()
		if idle {
			if err := m.unload(ctx, item, false); err != nil && !errors.Is(err, ErrInFlight) {
				result = append(result, err)
			}
		}
	}
	return errors.Join(result...)
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.accepting = false
	items := make([]*entry, 0, len(m.entries)+len(m.retired))
	for _, item := range m.entries {
		items = append(items, item)
	}
	items = append(items, m.retired...)
	m.mu.Unlock()
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
		item.mu.Unlock()
		if ready {
			if err := m.unload(ctx, item, true); err != nil {
				result = append(result, err)
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

func (m *Manager) Snapshot(id string) (Snapshot, error) {
	item, err := m.resolve(id)
	if err != nil {
		return Snapshot{}, err
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	return Snapshot{
		ID: item.manifest.ID, Version: item.manifest.Version, Mode: item.manifest.Mode,
		State: item.state, Pin: item.manifest.Pin, InFlight: item.inFlight, LastUsedAt: item.lastUsedAt,
	}, nil
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

func validateManifest(manifest Manifest) error {
	if !stableIDPattern.MatchString(manifest.ID) || !versionPattern.MatchString(manifest.Version) ||
		(manifest.Mode != ModeEmbedded && manifest.Mode != ModeHosted && manifest.Mode != ModeIsolated) ||
		manifest.IdleTTL < 0 || len(manifest.LockedDigest) != 64 {
		return ErrInvalidManifest
	}
	digest, err := hex.DecodeString(manifest.LockedDigest)
	if err != nil || len(digest) != 32 {
		return ErrInvalidManifest
	}
	return nil
}
