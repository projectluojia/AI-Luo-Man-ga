package campus

import (
	"encoding/json"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

const (
	AppID     = "campus-services"
	ServiceID = "campus"
	// BusComponentID 是 campus 包内导出全部校巴 Capability 的组件 ID。就绪校验
	// 与测试装配共用该常量：包内可以有其他组件，只有 bus 组件在位才算就绪。
	BusComponentID = "bus"

	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
)

// ToolSpecs 返回校巴工具规格（内核侧注册用；来源是 pkg/bus 的中立常量）。
func ToolSpecs() []capability.ToolSpec {
	return []capability.ToolSpec{
		{
			ID:              bus.StopSearchToolID,
			Version:         "1.0.0",
			Description:     "Search authoritative campus bus stops by name or alias.",
			InputSchemaJSON: bus.StopSearchInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
		},
		{
			ID:              bus.RouteListToolID,
			Version:         "1.0.0",
			Description:     "List authoritative campus bus routes.",
			InputSchemaJSON: bus.RouteListInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
		},
		{
			ID:              bus.JourneySearchToolID,
			Version:         "1.0.0",
			Description:     "Search authoritative campus bus journeys by stops and departure time.",
			InputSchemaJSON: bus.JourneySearchInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
		},
	}
}

// ServiceSpec 返回 campus 服务规格。
func ServiceSpec() capability.ServiceSpec {
	return capability.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Campus-wide public services.",
		ToolDependencies: []string{bus.StopSearchToolID, bus.RouteListToolID, bus.JourneySearchToolID},
	}
}

// CapabilitySpecs 返回 campus 对外暴露的 Capability 规格。
func CapabilitySpecs() []capability.CapabilitySpec {
	return []capability.CapabilitySpec{
		{
			ID:              BusStopSearchCapabilityID,
			Version:         "1.0.0",
			Name:            "查询校巴站点",
			Description:     "Search campus bus stops by a user-provided stop name or alias.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.StopSearchInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
			ToolID:          bus.StopSearchToolID,
		},
		{
			ID:              BusRouteListCapabilityID,
			Version:         "1.0.0",
			Name:            "列出校巴线路",
			Description:     "List currently available campus bus routes and directions.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.RouteListInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
			ToolID:          bus.RouteListToolID,
		},
		{
			ID:              BusJourneySearchCapabilityID,
			Version:         "1.0.0",
			Name:            "查询校巴行程",
			Description:     "Find direct campus bus journeys for stable origin and destination stop IDs and a departure time.",
			ServiceID:       ServiceID,
			InputSchemaJSON: bus.JourneySearchInputSchemaJSON,
			SideEffect:      capability.SideEffectRead,
			ToolID:          bus.JourneySearchToolID,
		},
	}
}

// Extensions 返回 campus 的 extensions 段（tools/service/capabilities JSON），
// 供安装包构造（campustest）与 SDK 生成（e2e）等消费方共用同一权威契约。
func Extensions() (json.RawMessage, error) {
	extensions, err := json.Marshal(struct {
		Tools        []capability.ToolSpec       `json:"tools"`
		Service      capability.ServiceSpec      `json:"service"`
		Capabilities []capability.CapabilitySpec `json:"capabilities"`
	}{Tools: ToolSpecs(), Service: ServiceSpec(), Capabilities: CapabilitySpecs()})
	if err != nil {
		return nil, err
	}
	return extensions, nil
}
