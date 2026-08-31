// Package calendar 暴露校历查询 Capability，并通过 Dispatcher 统一治理 Tool 调用。
package calendar

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
)

const (
	ServiceID    = "calendar"
	CapabilityID = cal.EventsListCapabilityID
)

type Service struct{ store cal.Store }

func NewService(store cal.Store) *Service { return &Service{store: store} }
func Register(reg *registry.Registry, store cal.Store) error {
	if reg == nil || store == nil {
		return registry.ErrInvalidSpec
	}
	handler := cal.ToolHandlers(store)[cal.EventsListToolID]
	capabilities := map[string]struct {
		Spec    registry.CapabilitySpec
		Handler registry.Handler
	}{}
	for _, id := range []string{CapabilityID, cal.AcademicCalendarQueryCapabilityID, cal.CalendarQueryCapabilityID} {
		capabilities[id] = struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{Spec: registry.CapabilitySpec{ID: id, Version: "1.0.0", Name: "查询武大校历", Description: "List Wuhan University academic calendar events.", ServiceID: ServiceID, InputSchemaJSON: cal.InputSchemaJSON, SideEffect: registry.SideEffectRead, ToolID: cal.EventsListToolID}, Handler: handler}
	}
	return reg.RegisterBatch(cal.ToolRegistrations(store), []registry.ServiceRegistration{{
		Spec:         registry.ServiceSpec{ID: ServiceID, Version: "1.0.0", Description: "Wuhan University academic calendar.", ToolDependencies: []string{cal.EventsListToolID}},
		Capabilities: capabilities,
	}})
}
