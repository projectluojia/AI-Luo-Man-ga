// Package sports 装配运动场馆预约 Service，并仅通过 Registry 暴露受治理 Capability。
package sports

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	sportstool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

const (
	ServiceID = "sports"

	VenuesListCapabilityID         = sportstool.VenueListToolID
	ProjectsListCapabilityID       = sportstool.ProjectListToolID
	SlotsSearchCapabilityID        = sportstool.SlotSearchToolID
	ReservationsCreateCapabilityID = sportstool.ReservationCreateToolID
	ReservationsCancelCapabilityID = sportstool.ReservationCancelToolID
	ReservationsMineCapabilityID   = sportstool.ReservationMineToolID
	OrdersWebViewCapabilityID      = sportstool.OrdersWebViewToolID
	ScheduleAddCapabilityID        = sportstool.ScheduleAddToolID
)

// CapabilityIDs 返回运动场馆预约对外暴露的全部 Capability 标识。
func CapabilityIDs() []string {
	return []string{
		VenuesListCapabilityID,
		ProjectsListCapabilityID,
		SlotsSearchCapabilityID,
		ReservationsCreateCapabilityID,
		ReservationsCancelCapabilityID,
		ReservationsMineCapabilityID,
		OrdersWebViewCapabilityID,
		ScheduleAddCapabilityID,
	}
}

// ServiceSpec 返回运动场馆 Service 的注册元数据。
func ServiceSpec() registry.ServiceSpec {
	dependencies := make([]string, 0, len(sportstool.ToolSpecs()))
	for _, spec := range sportstool.ToolSpecs() {
		dependencies = append(dependencies, spec.ID)
	}
	return registry.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Governed sports venue reservation with over-quota rejection and order WebView descriptors.",
		ToolDependencies: dependencies,
	}
}

// Service 是运动场馆预约 Service。实现状态全部由传入的 Go 存储端口管理。
type Service struct {
	store sportstool.Store
	now   func() time.Time
}

func NewService(store sportstool.Store) *Service {
	return NewServiceWithNow(store, time.Now)
}

// NewServiceWithNow 注入时钟，供测试固定时段与过期边界。
func NewServiceWithNow(store sportstool.Store, now func() time.Time) *Service {
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

// Register 原子注册运动场馆 Tool、Service 与 Capability。
func (s *Service) Register(reg *registry.Registry) error {
	if s == nil || reg == nil || s.store == nil {
		return registry.ErrInvalidSpec
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	handlers := sportstool.ToolHandlers(s.store, now)
	capability := func(id, name, description, schema, sideEffect string, confirm bool) struct {
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
				RequiresConfirmation: confirm, ToolID: id,
			},
			Handler: handlers[id],
		}
	}
	read := registry.SideEffectRead
	write := registry.SideEffectWrite
	return reg.RegisterBatch(sportstool.ToolRegistrations(s.store, now), []registry.ServiceRegistration{{
		Spec: ServiceSpec(),
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			VenuesListCapabilityID:         capability(VenuesListCapabilityID, "查询运动场馆", "List sports venues from the current governed snapshot.", sportstool.VenueListInputSchemaJSON, read, false),
			ProjectsListCapabilityID:       capability(ProjectsListCapabilityID, "查询运动项目", "List sports projects under a venue.", sportstool.ProjectListInputSchemaJSON, read, false),
			SlotsSearchCapabilityID:        capability(SlotsSearchCapabilityID, "查询运动时段", "Search sports reservation slots by venue, project and local date.", sportstool.SlotSearchInputSchemaJSON, read, false),
			ReservationsCreateCapabilityID: capability(ReservationsCreateCapabilityID, "预约运动场馆", "Create a sports venue reservation. Over-quota requests are rejected without truncation.", sportstool.ReservationCreateInputSchemaJSON, write, true),
			ReservationsCancelCapabilityID: capability(ReservationsCancelCapabilityID, "取消运动场馆预约", "Cancel a sports venue reservation owned by the current user.", sportstool.ReservationCancelInputSchemaJSON, write, true),
			ReservationsMineCapabilityID:   capability(ReservationsMineCapabilityID, "查询我的运动预约", "List sports reservations owned by the current user.", sportstool.ReservationMineInputSchemaJSON, read, false),
			OrdersWebViewCapabilityID:      capability(OrdersWebViewCapabilityID, "获取运动订单 WebView 描述符", "Return a governed sports order WebView session descriptor without secrets.", sportstool.OrdersWebViewInputSchemaJSON, read, false),
			ScheduleAddCapabilityID:        capability(ScheduleAddCapabilityID, "将运动预约加入日程", "Persist a local schedule item for a sports reservation.", sportstool.ScheduleAddInputSchemaJSON, write, true),
		},
	}})
}
