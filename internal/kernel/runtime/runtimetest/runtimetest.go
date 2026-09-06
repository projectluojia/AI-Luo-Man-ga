// Package runtimetest 提供 runtime 调度器的测试替身，仅测试代码引用。
// 生产 runtime 包不携带测试实现。
package runtimetest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

// StaticAppPolicy 是测试用的静态策略：按 App 显式 Enable/Grant，
// Snapshot 返回固定的能力与权限集合。
type StaticAppPolicy struct {
	mu     sync.RWMutex
	grants map[string][]capability.Grant
}

// NewStaticAppPolicy 构造空的静态策略。
func NewStaticAppPolicy() *StaticAppPolicy {
	return &StaticAppPolicy{grants: make(map[string][]capability.Grant)}
}

// Enable 允许 App 使用指定能力。
func (p *StaticAppPolicy) Enable(appID, capabilityID string) {
	p.EnableResource(appID, capabilityID, "any", nil)
}

// EnableResource 为测试指定 Capability 的资源范围。
func (p *StaticAppPolicy) EnableResource(appID, capabilityID, resourceType string, resourceIDs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants[appID] = append(p.grants[appID], capability.Grant{
		ID: "grant-" + capabilityID, AppID: appID, Principal: capability.PrincipalAny, CapabilityID: capabilityID,
		Resource:  capability.ResourceScope{Type: resourceType, IDs: append([]string(nil), resourceIDs...)},
		ExpiresAt: time.Now().Add(24 * time.Hour), MaxCalls: 1000,
		PolicyRevision: "static",
	})
}

// Snapshot 返回静态策略快照（能力与权限按规范排序）。
func (p *StaticAppPolicy) Snapshot(_ context.Context, appID string) (appconfig.PolicySnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	grants := append([]capability.Grant(nil), p.grants[appID]...)
	sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
	return appconfig.PolicySnapshot{
		AppID: appID, Revision: "static", Generation: 1,
		Enabled:          true,
		CapabilityGrants: grants,
	}, nil
}
