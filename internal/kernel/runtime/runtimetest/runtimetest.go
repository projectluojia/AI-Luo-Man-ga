// Package runtimetest 提供 runtime 调度器的测试替身，仅测试代码引用。
// 生产 runtime 包不携带测试实现。
package runtimetest

import (
	"context"
	"sort"
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

// StaticAppPolicy 是测试用的静态策略：按 App 显式 Enable/Grant，
// Snapshot 返回固定的能力与权限集合。
type StaticAppPolicy struct {
	mu          sync.RWMutex
	enabled     map[string]map[string]struct{}
	permissions map[string]map[string]struct{}
}

// NewStaticAppPolicy 构造空的静态策略。
func NewStaticAppPolicy() *StaticAppPolicy {
	return &StaticAppPolicy{
		enabled: make(map[string]map[string]struct{}), permissions: make(map[string]map[string]struct{}),
	}
}

// Enable 允许 App 使用指定能力。
func (p *StaticAppPolicy) Enable(appID, capabilityID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enabled[appID] == nil {
		p.enabled[appID] = make(map[string]struct{})
	}
	p.enabled[appID][capabilityID] = struct{}{}
}

// Grant 授予 App 一项权限。
func (p *StaticAppPolicy) Grant(appID, permission string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.permissions[appID] == nil {
		p.permissions[appID] = make(map[string]struct{})
	}
	p.permissions[appID][permission] = struct{}{}
}

// Snapshot 返回静态策略快照（能力与权限按规范排序）。
func (p *StaticAppPolicy) Snapshot(_ context.Context, appID string) (appconfig.PolicySnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	capabilities := make([]string, 0, len(p.enabled[appID]))
	for capability := range p.enabled[appID] {
		capabilities = append(capabilities, capability)
	}
	permissions := make([]string, 0, len(p.permissions[appID]))
	for permission := range p.permissions[appID] {
		permissions = append(permissions, permission)
	}
	sort.Strings(capabilities)
	sort.Strings(permissions)
	return appconfig.PolicySnapshot{
		AppID: appID, Revision: "static", Generation: 1,
		Enabled:             true,
		EnabledCapabilities: capabilities, PermissionScope: permissions,
	}, nil
}
