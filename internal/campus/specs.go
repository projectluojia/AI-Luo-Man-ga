package campus

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	AppID     = "campus-services"
	ServiceID = "campus"

	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
)

// ToolSpecs 返回 campus 服务依赖的工具规格（单一来源：安装清单与 Registry 注册共用）。
func ToolSpecs() []registry.ToolSpec {
	return bus.ToolSpecs()
}

// ServiceSpec 返回 campus 服务规格。
func ServiceSpec() registry.ServiceSpec {
	return registry.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Campus-wide public services.",
		ToolDependencies: []string{bus.StopSearchToolID, bus.RouteListToolID, bus.JourneySearchToolID},
	}
}

// CapabilitySpecs 返回 campus 对外暴露的 Capability 规格。
func CapabilitySpecs() []registry.CapabilitySpec {
	return []registry.CapabilitySpec{
		{
			ID:              BusStopSearchCapabilityID,
			Version:         "1.0.0",
			Name:            "查询校巴站点",
			Description:     "Search campus bus stops by a user-provided stop name or alias.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.StopSearchInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
			ToolID:          bus.StopSearchToolID,
		},
		{
			ID:              BusRouteListCapabilityID,
			Version:         "1.0.0",
			Name:            "列出校巴线路",
			Description:     "List currently available campus bus routes and directions.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.RouteListInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
			ToolID:          bus.RouteListToolID,
		},
		{
			ID:              BusJourneySearchCapabilityID,
			Version:         "1.0.0",
			Name:            "查询校巴行程",
			Description:     "Find direct campus bus journeys for stable origin and destination stop IDs and a departure time.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.JourneySearchInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
			ToolID:          bus.JourneySearchToolID,
		},
	}
}
