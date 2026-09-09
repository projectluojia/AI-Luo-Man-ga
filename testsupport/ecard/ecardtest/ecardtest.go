// Package ecardtest 提供珞珈 E 卡 hosted 包装配的共享测试辅助。
//
// 装配机制委托共享的 hostedtest：读仓库内真实 packages/ecard 清单
// （ailuo.toml）与源码（src/、app/、ecard/、guestkit），packagefmt go-wasm
// 现场编译——清单与 guest 的测试即生产形态。凭据为个人作用域文档（治理
// 上下文注入 UserID）；入口目录是包内演示常量，无快照播种。
package ecardtest

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// 包与组件标识（与 packages/ecard/ailuo.toml 一致，漂移由清单解析测试兜底）。
const (
	PackageID        = "ecard"
	ComponentID      = "provider"
	StorageNamespace = "ecard/credentials"
)

// Capability 标识（与清单 exports 一致）。
const (
	EntriesListCapabilityID       = "ecard.entries.list"
	SessionPrepareCapabilityID    = "ecard.session.prepare"
	CredentialsPutCapabilityID    = "ecard.credentials.put"
	CredentialsRevokeCapabilityID = "ecard.credentials.revoke"
	CredentialsStatusCapabilityID = "ecard.credentials.status"
)

// CapabilityIDs 返回全部能力标识（测试批量启用策略用）。
func CapabilityIDs() []string {
	return []string{
		EntriesListCapabilityID, SessionPrepareCapabilityID, CredentialsPutCapabilityID,
		CredentialsRevokeCapabilityID, CredentialsStatusCapabilityID,
	}
}

// RegisterHosted 以 hosted 包形态装配 E 卡包：真实清单 + 真实 guest 源码，
// ailuo.store 宿主函数绑定到 packstore 端口（凭据按 UserID 隔离），与生产
// 安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, hostedtest.Spec{
		Dir: "ecard", ComponentID: ComponentID, StorageNamespace: StorageNamespace,
	})
}
