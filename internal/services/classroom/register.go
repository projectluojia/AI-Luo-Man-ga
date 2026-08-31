// Package classroom 装配空闲教室 Service，并仅通过 Registry 暴露受治理 Capability。
package classroom

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	classroomtool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

const (
	ServiceID = "classroom"

	RoomsSearchCapabilityID    = classroomtool.RoomsSearchToolID
	CampusesListCapabilityID   = classroomtool.CampusesListToolID
	BuildingsListCapabilityID  = classroomtool.BuildingsListToolID
	ScheduleCreateCapabilityID = classroomtool.ScheduleCreateToolID
	ScheduleListCapabilityID   = classroomtool.ScheduleListToolID
	ScheduleCancelCapabilityID = classroomtool.ScheduleCancelToolID
)

// CapabilityIDs 返回教室 Service 对外暴露的全部 Capability 标识。
func CapabilityIDs() []string {
	return []string{
		RoomsSearchCapabilityID,
		CampusesListCapabilityID,
		BuildingsListCapabilityID,
		ScheduleCreateCapabilityID,
		ScheduleListCapabilityID,
		ScheduleCancelCapabilityID,
	}
}

// ServiceSpec 返回教室 Service 的注册元数据。
func ServiceSpec() registry.ServiceSpec {
	dependencies := make([]string, 0, len(classroomtool.ToolSpecs()))
	for _, spec := range classroomtool.ToolSpecs() {
		dependencies = append(dependencies, spec.ID)
	}
	return registry.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Empty classroom lookup with governed snapshots and local schedule items.",
		ToolDependencies: dependencies,
	}
}

// Service 把教室存储端口装配为 Capability 处理器。
type Service struct {
	store classroomtool.Store
	now   func() time.Time
}

func NewService(store classroomtool.Store) *Service {
	return NewServiceWithClock(store, time.Now)
}

func NewServiceWithClock(store classroomtool.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

// Register 是与其他 L3 Service 一致的便捷装配入口。
func Register(reg *registry.Registry, service *Service) error {
	if service == nil {
		return registry.ErrInvalidSpec
	}
	return service.Register(reg)
}

// Register 原子注册教室 Tool、Service 与 Capability。Capability 处理器复用同一
// Tool handler，调用仍由 Dispatcher 统一做 App 策略、权限、Schema、幂等和确认。
func (s *Service) Register(reg *registry.Registry) error {
	if s == nil || reg == nil || s.store == nil {
		return registry.ErrInvalidSpec
	}
	handlers := classroomtool.ToolHandlers(s.store, s.now)
	capability := func(id, toolID, name, description, schema, sideEffect string, confirm bool) struct {
		Spec    registry.CapabilitySpec
		Handler registry.Handler
	} {
		return struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			Spec: registry.CapabilitySpec{
				ID: id, Version: "1.0.0", Name: name, Description: description,
				ServiceID: ServiceID, InputSchemaJSON: schema, SideEffect: sideEffect,
				RequiresConfirmation: confirm, ToolID: toolID,
			},
			Handler: handlers[toolID],
		}
	}
	read := registry.SideEffectRead
	write := registry.SideEffectWrite
	return reg.RegisterBatch(classroomtool.ToolRegistrations(s.store, s.now), []registry.ServiceRegistration{{
		Spec: ServiceSpec(),
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			RoomsSearchCapabilityID:    capability(RoomsSearchCapabilityID, classroomtool.RoomsSearchToolID, "查询空闲教室", "Search empty classrooms by academic date, campus, optional building and period 1-13.", classroomtool.RoomsSearchInputSchemaJSON, read, false),
			CampusesListCapabilityID:   capability(CampusesListCapabilityID, classroomtool.CampusesListToolID, "列出校区", "List campuses from the current classroom snapshot.", classroomtool.CampusesListInputSchemaJSON, read, false),
			BuildingsListCapabilityID:  capability(BuildingsListCapabilityID, classroomtool.BuildingsListToolID, "列出教学楼", "List buildings for a campus from the current classroom snapshot.", classroomtool.BuildingsListInputSchemaJSON, read, false),
			ScheduleCreateCapabilityID: capability(ScheduleCreateCapabilityID, classroomtool.ScheduleCreateToolID, "创建教室日程", "Create a local classroom schedule item from a room, date and period.", classroomtool.ScheduleCreateInputSchemaJSON, write, true),
			ScheduleListCapabilityID:   capability(ScheduleListCapabilityID, classroomtool.ScheduleListToolID, "查看教室日程", "List classroom schedule items owned by the current user.", classroomtool.ScheduleListInputSchemaJSON, read, false),
			ScheduleCancelCapabilityID: capability(ScheduleCancelCapabilityID, classroomtool.ScheduleCancelToolID, "取消教室日程", "Cancel a classroom schedule item owned by the current user.", classroomtool.ScheduleCancelInputSchemaJSON, write, true),
		},
	}})
}
