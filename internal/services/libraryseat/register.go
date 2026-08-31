// Package libraryseat 装配图书馆座位预约 Service，并仅通过 Registry 暴露受治理 Capability。
package libraryseat

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	libraryseattool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

const (
	ServiceID = "libraryseat"

	SpacesListCapabilityID         = libraryseattool.SpacesListToolID
	SlotsSearchCapabilityID        = libraryseattool.SlotsSearchToolID
	ReservationsCreateCapabilityID = libraryseattool.ReservationsCreateToolID
	ReservationsCancelCapabilityID = libraryseattool.ReservationsCancelToolID
	ReservationsMineCapabilityID   = libraryseattool.ReservationsMineToolID
)

// CapabilityIDs 返回本 Service 对外暴露的全部 Capability 标识。
func CapabilityIDs() []string {
	return []string{
		SpacesListCapabilityID,
		SlotsSearchCapabilityID,
		ReservationsCreateCapabilityID,
		ReservationsCancelCapabilityID,
		ReservationsMineCapabilityID,
	}
}

// ServiceSpec 返回座位预约 Service 的注册元数据。
func ServiceSpec() registry.ServiceSpec {
	dependencies := make([]string, 0, len(libraryseattool.ToolSpecs()))
	for _, spec := range libraryseattool.ToolSpecs() {
		dependencies = append(dependencies, spec.ID)
	}
	return registry.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Library seat reservation over a governed catalog snapshot and local reservation state machine.",
		ToolDependencies: dependencies,
	}
}

// Service 是座位预约业务组合：状态全部由 Go 存储端口管理。
type Service struct {
	store libraryseattool.Store
	now   func() time.Time
}

func NewService(store libraryseattool.Store) *Service {
	return NewServiceWithClock(store, time.Now)
}

func NewServiceWithClock(store libraryseattool.Store, now func() time.Time) *Service {
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

// Register 原子注册座位预约 Tool、Service 与 Capability。
// Capability 处理器复用同一 Tool handler，调用仍由 Dispatcher 统一做 App 策略、
// 权限、Schema、幂等、确认和审计。
func (s *Service) Register(reg *registry.Registry) error {
	if s == nil || reg == nil || s.store == nil {
		return registry.ErrInvalidSpec
	}
	handlers := libraryseattool.ToolHandlers(s.store, s.now)
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
	return reg.RegisterBatch(libraryseattool.ToolRegistrations(s.store, s.now), []registry.ServiceRegistration{{
		Spec: ServiceSpec(),
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			SpacesListCapabilityID: capability(SpacesListCapabilityID, libraryseattool.SpacesListToolID, "列出图书馆空间",
				"List library spaces in the current catalog snapshot. Demo catalogs are marked non-authoritative.", libraryseattool.SpacesListInputSchemaJSON, read, false),
			SlotsSearchCapabilityID: capability(SlotsSearchCapabilityID, libraryseattool.SlotsSearchToolID, "查询座位时段",
				"Search seats by space, Asia/Shanghai date and optional slot. Occupancy comes from local reservations, not live 智慧珞珈 data.", libraryseattool.SlotsSearchInputSchemaJSON, read, false),
			ReservationsCreateCapabilityID: capability(ReservationsCreateCapabilityID, libraryseattool.ReservationsCreateToolID, "预约图书馆座位",
				"Hold a seat for the current user in one space/date/slot. Rejects double-booking and a small per-user quota.", libraryseattool.ReservationsCreateInputSchemaJSON, write, true),
			ReservationsCancelCapabilityID: capability(ReservationsCancelCapabilityID, libraryseattool.ReservationsCancelToolID, "取消座位预约",
				"Cancel a reservation owned by the current user in the same App.", libraryseattool.ReservationsCancelInputSchemaJSON, write, true),
			ReservationsMineCapabilityID: capability(ReservationsMineCapabilityID, libraryseattool.ReservationsMineToolID, "我的座位预约",
				"List library seat reservations owned by the current user.", libraryseattool.ReservationsMineInputSchemaJSON, read, false),
		},
	}})
}
