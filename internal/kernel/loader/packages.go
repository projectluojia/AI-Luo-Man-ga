package loader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// PackageSpec 是一个包的候选升级清单（组件按依赖顺序）。
type PackageSpec struct {
	ID         string
	Components []ComponentSpec
}

// ComponentSpec 是单个组件的候选运行时清单。
type ComponentSpec struct {
	Runtime Manifest
}

// packageCandidate 是升级候选的加载结果。
type packageCandidate struct {
	runtime Runtime
	host    Host
}

// retiredGroup 是升级后旧版本组件的退役组：全部 inFlight 归零后反序停止。
type retiredGroup struct {
	mu        sync.Mutex
	draining  int
	triggered bool
	order     []*retiredRuntime // 反序（停止顺序）
}

// memberDrained 由 Release 在退役成员排空时调用，每个成员只调用一次。
func (g *retiredGroup) memberDrained() {
	g.mu.Lock()
	g.draining--
	g.mu.Unlock()
	g.stopIfDrained()
}

func (g *retiredGroup) stopIfDrained() {
	g.mu.Lock()
	if g.triggered || g.draining > 0 {
		g.mu.Unlock()
		return
	}
	g.triggered = true
	g.mu.Unlock()
	for _, member := range g.order {
		stopRetiredRuntime(member)
	}
}

// RegisterPackages 一次性提交多个包的组件分组。调用方已经完成清单和运行时
// 校验；本方法仍在同一锁内检查冲突，避免只发布部分包组。
func (m *Manager) RegisterPackages(groups map[string][]string) error {
	if len(groups) == 0 {
		return ErrInvalidManifest
	}
	packageIDs := make([]string, 0, len(groups))
	resolved := make(map[string]*packageGroup, len(groups))
	for packageID, componentIDs := range groups {
		if packageID == "" || len(componentIDs) == 0 {
			return ErrInvalidManifest
		}
		group := &packageGroup{}
		for _, componentID := range componentIDs {
			item, err := m.resolve(componentID)
			if err != nil {
				return err
			}
			group.order = append(group.order, item)
		}
		packageIDs = append(packageIDs, packageID)
		resolved[packageID] = group
	}
	sort.Strings(packageIDs)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return ErrShuttingDown
	}
	for _, packageID := range packageIDs {
		if _, exists := m.packages[packageID]; exists {
			return ErrDuplicateID
		}
	}
	for _, packageID := range packageIDs {
		m.packages[packageID] = resolved[packageID]
	}
	return nil
}

// UpgradePackage 以整个包为单位原子升级：候选组件按依赖顺序加载完成后，
// 全部就绪后原子切换，旧版本组件在组级 inFlight 全部归零后反序停止。
// 任一候选失败 → 不切换（旧版本原样保留）。
func (m *Manager) UpgradePackage(ctx context.Context, spec PackageSpec) error {
	if err := validatePackageSpec(spec); err != nil {
		return err
	}
	if err := m.beginUpgrade(); err != nil {
		return err
	}
	defer m.endUpgrade()
	m.mu.RLock()
	group := m.packages[spec.ID]
	m.mu.RUnlock()
	if group == nil {
		return fmt.Errorf("%w: package %q not registered", ErrNotFound, spec.ID)
	}
	group.upgradeMu.Lock()
	defer group.upgradeMu.Unlock()
	if len(spec.Components) != len(group.order) {
		return ErrInvalidManifest
	}
	// 1. 按依赖顺序加载候选组件。
	candidates := make([]packageCandidate, 0, len(spec.Components))
	for index, component := range spec.Components {
		item := group.order[index]
		if component.Runtime.ID != item.manifest.ID {
			stopCandidates(candidates)
			return ErrInvalidManifest
		}
		host, err := m.selectHost(ctx, component.Runtime)
		if err != nil {
			stopCandidates(candidates)
			return err
		}
		started := m.now()
		result := loadRuntime(ctx, host, component.Runtime)
		observe.DefaultMetrics().ObserveRuntimeLoad(result.err == nil, m.now().Sub(started))
		if result.err != nil {
			stopCandidates(candidates)
			return errors.Join(ErrLoadFailed, result.err)
		}
		candidates = append(candidates, packageCandidate{runtime: result.runtime, host: host})
	}
	// 2. 原子切换：锁定全部条目，校验就绪后整体替换。
	entries := group.order
	for _, item := range entries {
		item.mu.Lock()
	}
	abort := func() {
		for _, item := range entries {
			item.mu.Unlock()
		}
		stopCandidates(candidates)
	}
	for index, item := range entries {
		if item.state != StateReady || item.runtime == nil {
			abort()
			return ErrUnavailable
		}
		if item.manifest.Version == spec.Components[index].Runtime.Version {
			abort()
			return ErrInvalidManifest
		}
	}
	// 建立退役组。
	retired := &retiredGroup{}
	for index, item := range entries {
		old := &retiredRuntime{
			runtime:    item.runtime,
			generation: item.generation,
			inFlight:   item.currentInFlight,
			stopped:    make(chan struct{}),
			group:      retired,
		}
		item.retired = append(item.retired, old)
		item.manifest = spec.Components[index].Runtime
		item.host = candidates[index].host
		item.runtime = candidates[index].runtime
		item.generation++
		item.currentInFlight = 0
		if old.inFlight > 0 {
			retired.draining++
		}
	}
	// 反序：停止顺序 = 消费者先停。
	for i := len(entries) - 1; i >= 0; i-- {
		item := entries[i]
		old := item.retired[len(item.retired)-1]
		retired.order = append(retired.order, old)
	}
	for _, item := range entries {
		item.mu.Unlock()
	}
	observe.Info(ctx, "包已原子升级",
		observe.StringAttr("package_id", spec.ID),
		observe.StringAttr("package_version", spec.Components[0].Runtime.Version),
	)
	retired.stopIfDrained()
	return nil
}

func validatePackageSpec(spec PackageSpec) error {
	if spec.ID == "" || len(spec.Components) == 0 || len(spec.Components) > 64 {
		return ErrInvalidManifest
	}
	for _, component := range spec.Components {
		if err := ValidateManifest(component.Runtime); err != nil {
			return err
		}
	}
	return nil
}

func stopCandidates(candidates []packageCandidate) {
	for _, c := range candidates {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
		_ = c.runtime.Stop(ctx)
		cancel()
	}
}
