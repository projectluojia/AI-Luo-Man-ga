// Package timetable 装配课表 Service，并仅通过 Registry 暴露受治理 Capability。
package timetable

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	timetabletool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/timetable"
)

const (
	ServiceID = "timetable"

	ListCapabilityID         = "timetable.list"
	GetCapabilityID          = "timetable.get"
	CreateCapabilityID       = "timetable.create"
	UpdateCapabilityID       = "timetable.update"
	DeleteCapabilityID       = "timetable.delete"
	ActivateCapabilityID     = "timetable.activate"
	CourseListCapabilityID   = "timetable.courses.list"
	CourseGetCapabilityID    = "timetable.course.get"
	CourseCreateCapabilityID = "timetable.course.create"
	CourseUpdateCapabilityID = "timetable.course.update"
	CourseDeleteCapabilityID = "timetable.course.delete"
	ImportCapabilityID       = "timetable.import"
)

// ServiceSpec 返回课表 Service 的注册元数据。
func ServiceSpec() registry.ServiceSpec {
	dependencies := make([]string, 0, len(timetabletool.ToolSpecs()))
	for _, spec := range timetabletool.ToolSpecs() {
		dependencies = append(dependencies, spec.ID)
	}
	return registry.ServiceSpec{ID: ServiceID, Version: "1.0.0", Description: "User-owned timetables with governed imports and local course editing.", ToolDependencies: dependencies}
}

// NewService 返回课表 Service。实现状态全部由传入的 Go 存储端口管理。
type Service struct{ store timetabletool.Store }

func NewService(store timetabletool.Store) *Service { return &Service{store: store} }

// Register 是与其他 L3 Service 一致的便捷装配入口。
func Register(reg *registry.Registry, service *Service) error {
	if service == nil {
		return registry.ErrInvalidSpec
	}
	return service.Register(reg)
}

// Register 原子注册课表 Tool、Service 与 Capability。Capability 处理器复用同一
// Tool handler，调用仍由 Dispatcher 统一做 App 策略、权限、Schema、幂等和审计。
func (s *Service) Register(reg *registry.Registry) error {
	if s == nil || reg == nil || s.store == nil {
		return registry.ErrInvalidSpec
	}
	handlers := timetabletool.ToolHandlers(s.store)
	capability := func(id, toolID, name, description, schema, sideEffect string) struct {
		Spec    registry.CapabilitySpec
		Handler registry.Handler
	} {
		return struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			Spec:    registry.CapabilitySpec{ID: id, Version: "1.0.0", Name: name, Description: description, ServiceID: ServiceID, InputSchemaJSON: schema, SideEffect: sideEffect, ToolID: toolID},
			Handler: handlers[toolID],
		}
	}
	read := registry.SideEffectRead
	write := registry.SideEffectWrite
	return reg.RegisterBatch(timetabletool.ToolRegistrations(s.store), []registry.ServiceRegistration{{
		Spec: ServiceSpec(),
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			ListCapabilityID:         capability(ListCapabilityID, timetabletool.TimetableListToolID, "查看我的课表", "List all timetables owned by the current user.", timetabletool.TimetableListInputSchemaJSONExport, read),
			GetCapabilityID:          capability(GetCapabilityID, timetabletool.TimetableGetToolID, "查看一份课表", "Get a timetable owned by the current user.", timetabletool.TimetableIDInputSchemaJSONExport, read),
			CreateCapabilityID:       capability(CreateCapabilityID, timetabletool.TimetableCreateToolID, "创建本地课表", "Create a local timetable.", timetabletool.TimetableCreateInputSchemaJSONExport, write),
			UpdateCapabilityID:       capability(UpdateCapabilityID, timetabletool.TimetableUpdateToolID, "编辑课表", "Rename or activate a timetable.", timetabletool.TimetableUpdateInputSchemaJSONExport, write),
			DeleteCapabilityID:       capability(DeleteCapabilityID, timetabletool.TimetableDeleteToolID, "删除课表", "Delete a timetable and all its courses.", timetabletool.TimetableIDInputSchemaJSONExport, write),
			ActivateCapabilityID:     capability(ActivateCapabilityID, timetabletool.TimetableActivateToolID, "切换当前课表", "Make one timetable active for the current user.", timetabletool.TimetableIDInputSchemaJSONExport, write),
			CourseListCapabilityID:   capability(CourseListCapabilityID, timetabletool.CourseListToolID, "查看课程", "List courses in a timetable.", timetabletool.TimetableIDInputSchemaJSONExport, read),
			CourseGetCapabilityID:    capability(CourseGetCapabilityID, timetabletool.CourseGetToolID, "查看一门课程", "Get one course.", timetabletool.CourseGetInputSchemaJSONExport, read),
			CourseCreateCapabilityID: capability(CourseCreateCapabilityID, timetabletool.CourseCreateToolID, "新增课程", "Create a locally edited course.", timetabletool.CourseInputSchemaJSONExport, write),
			CourseUpdateCapabilityID: capability(CourseUpdateCapabilityID, timetabletool.CourseUpdateToolID, "编辑课程", "Replace a course.", timetabletool.CourseInputSchemaJSONExport, write),
			CourseDeleteCapabilityID: capability(CourseDeleteCapabilityID, timetabletool.CourseDeleteToolID, "删除课程", "Delete a course.", timetabletool.CourseGetInputSchemaJSONExport, write),
			ImportCapabilityID:       capability(ImportCapabilityID, timetabletool.TimetableImportToolID, "导入课表", "Import WuDa academic or WakeUp timetable content supplied by the user.", timetabletool.ImportInputSchemaJSONExport, write),
		},
	}})
}
