// Package campustoolstest 提供空闲教室与学业校历 hosted 包装配的共享测试辅助。
//
// 与 timetabletest/campustest 同模式：不做 guest 源码复制，装配机制委托共享
// 的 hostedtest（读仓库内真实 packages/classroom、packages/calendar 清单与源
// 码，packagefmt go-wasm 现场编译——清单与 guest 的测试即生产形态）。快照数
// 据由测试经 packstore.Store 播种（App 隔离由 Scope 强制），日程写路径由治
// 理上下文注入 UserID。
package campustoolstest

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// packageSpec 描述一个被装配的 hosted 包：仓库内目录与测试夹具标识。
type packageSpec = hostedtest.Spec

var (
	classroomSpec = packageSpec{Dir: "classroom", ComponentID: "provider", StorageNamespace: "classroom/rooms"}
	calendarSpec  = packageSpec{Dir: "calendar", ComponentID: "provider", StorageNamespace: "calendar/events"}
)

// Classroom 包与组件标识（与 packages/classroom/ailuo.toml 一致）。
const (
	ClassroomPackageID        = "classroom"
	ClassroomComponentID      = "provider"
	ClassroomStorageNamespace = "classroom/rooms"
)

// Classroom Capability 标识（与清单 exports 一致，漂移由清单解析测试兜底）。
const (
	ClassroomRoomsSearchCapabilityID    = "classroom.rooms.search"
	ClassroomCampusesListCapabilityID   = "classroom.campuses.list"
	ClassroomBuildingsListCapabilityID  = "classroom.buildings.list"
	ClassroomScheduleCreateCapabilityID = "classroom.schedule.create"
	ClassroomScheduleListCapabilityID   = "classroom.schedule.list"
	ClassroomScheduleCancelCapabilityID = "classroom.schedule.cancel"
)

// Calendar 包与能力标识（与 packages/calendar/ailuo.toml 一致）。
const (
	CalendarPackageID              = "calendar"
	CalendarComponentID            = "provider"
	CalendarStorageNamespace       = "calendar/events"
	CalendarEventsListCapabilityID = "calendar.events.list"
)

// ClassroomCapabilityIDs 返回教室全部能力标识（测试批量启用策略用）。
func ClassroomCapabilityIDs() []string {
	return []string{
		ClassroomRoomsSearchCapabilityID, ClassroomCampusesListCapabilityID,
		ClassroomBuildingsListCapabilityID, ClassroomScheduleCreateCapabilityID,
		ClassroomScheduleListCapabilityID, ClassroomScheduleCancelCapabilityID,
	}
}

// RegisterClassroomHosted 以 hosted 包形态装配空闲教室包：真实清单 + 真实
// guest 源码，ailuo.store 宿主函数绑定到 packstore 端口，与生产安装包链路一致。
func RegisterClassroomHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, classroomSpec)
}

// RegisterCalendarHosted 以 hosted 包形态装配学业校历包。
func RegisterCalendarHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, calendarSpec)
}
