// Package sportstest 提供运动场馆 hosted 包装配的共享测试辅助。
//
// 装配机制委托共享的 hostedtest：读仓库内真实 packages/sports 清单
// （ailuo.toml）与源码（src/、app/、sports/、guestkit），packagefmt go-wasm
// 现场编译——清单与 guest 的测试即生产形态。目录快照由测试经 packstore 播种
// （系统作用域），预约与日程为个人作用域写路径（治理上下文注入 UserID）。
package sportstest

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// 包与组件标识（与 packages/sports/ailuo.toml 一致，漂移由清单解析测试兜底）。
const (
	PackageID        = "sports"
	ComponentID      = "provider"
	StorageNamespace = "sports/venues"
)

// Capability 标识（与清单 exports 一致）。
const (
	VenuesListCapabilityID         = "sports.venues.list"
	ProjectsListCapabilityID       = "sports.projects.list"
	SlotsSearchCapabilityID        = "sports.slots.search"
	ReservationsCreateCapabilityID = "sports.reservations.create"
	ReservationsCancelCapabilityID = "sports.reservations.cancel"
	ReservationsMineCapabilityID   = "sports.reservations.mine"
	OrdersWebviewCapabilityID      = "sports.orders.webview"
	ScheduleAddCapabilityID        = "sports.schedule.add"
)

// CapabilityIDs 返回全部能力标识（测试批量启用策略用）。
func CapabilityIDs() []string {
	return []string{
		VenuesListCapabilityID, ProjectsListCapabilityID, SlotsSearchCapabilityID,
		ReservationsCreateCapabilityID, ReservationsCancelCapabilityID,
		ReservationsMineCapabilityID, OrdersWebviewCapabilityID, ScheduleAddCapabilityID,
	}
}

// RegisterHosted 以 hosted 包形态装配运动场馆包：真实清单 + 真实 guest 源码，
// ailuo.store 宿主函数绑定到 packstore 端口（预约按 UserID 隔离），与生产
// 安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, hostedtest.Spec{
		Dir: "sports", ComponentID: ComponentID, StorageNamespace: StorageNamespace,
	})
}
