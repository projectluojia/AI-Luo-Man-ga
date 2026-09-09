// Package timetabletest 提供课表 hosted 包装配的共享测试辅助。
//
// 装配机制委托共享的 hostedtest：不做 guest 源码复制，读仓库内真实
// packages/timetable 清单（ailuo.toml）与源码（src/、tt/、app/、guestkit），
// packagefmt go-wasm 现场编译——清单与 guest 的测试即生产形态。课表数据全
// 部落在个人作用域，测试调用方需携带已认证 UserID。
package timetabletest

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// PackageID / PackageVersion / StorageNamespace 与 packages/timetable/ailuo.toml
// 保持一致（清单解析测试会校验二者不漂移）。
const (
	PackageID        = "timetable"
	PackageVersion   = "1.0.0"
	StorageNamespace = "timetable/tables"
	ComponentID      = "provider"
)

// CapabilityID 是 12 个课表能力标识（与清单 exports 一致）。
const (
	CapabilityTimetableList     = "timetable.list"
	CapabilityTimetableGet      = "timetable.get"
	CapabilityTimetableCreate   = "timetable.create"
	CapabilityTimetableUpdate   = "timetable.update"
	CapabilityTimetableDelete   = "timetable.delete"
	CapabilityTimetableActivate = "timetable.activate"
	CapabilityCourseList        = "timetable.course.list"
	CapabilityCourseGet         = "timetable.course.get"
	CapabilityCourseCreate      = "timetable.course.create"
	CapabilityCourseUpdate      = "timetable.course.update"
	CapabilityCourseDelete      = "timetable.course.delete"
	CapabilityImport            = "timetable.import"
)

// CapabilityIDs 返回全部能力标识（测试批量启用策略用）。
func CapabilityIDs() []string {
	return []string{
		CapabilityTimetableList, CapabilityTimetableGet, CapabilityTimetableCreate,
		CapabilityTimetableUpdate, CapabilityTimetableDelete, CapabilityTimetableActivate,
		CapabilityCourseList, CapabilityCourseGet, CapabilityCourseCreate,
		CapabilityCourseUpdate, CapabilityCourseDelete, CapabilityImport,
	}
}

// RegisterHosted 以 hosted 包形态装配课表包：真实清单 + 真实 guest 源码，
// ailuo.store 宿主函数绑定到 packstore 端口（用户作用域由治理上下文注入
// UserID），与生产安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, hostedtest.Spec{
		Dir: "timetable", ComponentID: ComponentID, StorageNamespace: StorageNamespace,
	})
}
