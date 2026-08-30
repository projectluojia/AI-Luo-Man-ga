package campus

import (
	"encoding/json"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

// campus App 的部署常量与包契约标识。业务实现已迁至独立包仓库
// ailuo-packages/campus-bus（guest 自包含，经 ailuo pack/install 分发）；
// 本文件保留内核侧需要的稳定标识与契约夹具构造器：App 配置按 ID 启用能力，
// campustest 装配与 SDK 生成测试共用同一权威契约副本。工具 schema 的权威
// 来源是包仓库的 ailuo.toml。
const (
	AppID     = "campus-services"
	ServiceID = "campus"
	// BusComponentID 是 campus 包内导出全部校巴 Capability 的组件 ID。就绪校验
	// 与测试装配共用该常量：包内可以有其他组件，只有 bus 组件在位才算就绪。
	BusComponentID = "bus"

	// StorageNamespace 是包清单 [storage] 声明的持久化命名空间。
	StorageNamespace = "campus/bus"

	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
)

// 包内工具的稳定 ID 与输入 schema（与 ailuo-packages/campus-bus 的 ailuo.toml
// 保持一致的测试/部署夹具副本）。
const (
	BusStopSearchToolID    = "campus.bus.stops.search"
	BusRouteListToolID     = "campus.bus.routes.list"
	BusJourneySearchToolID = "campus.bus.journeys.search"

	BusStopSearchInputSchemaJSON    = `{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["query"],"additionalProperties":false}`
	BusRouteListInputSchemaJSON     = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	BusJourneySearchInputSchemaJSON = `{"type":"object","properties":{"origin_stop_id":{"type":"string","minLength":1},"destination_stop_id":{"type":"string","minLength":1},"depart_after":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["origin_stop_id","destination_stop_id"],"additionalProperties":false}`
)

// ToolSpecs 返回校巴工具规格（campustest 装配与 SDK 生成测试用）。
func ToolSpecs() []capability.ToolSpec {
	return []capability.ToolSpec{
		{
			ID: BusStopSearchToolID, Version: PackageVersion,
			Description:     "Search authoritative campus bus stops by name or alias.",
			InputSchemaJSON: BusStopSearchInputSchemaJSON, SideEffect: capability.SideEffectRead,
		},
		{
			ID: BusRouteListToolID, Version: PackageVersion,
			Description:     "List authoritative campus bus routes.",
			InputSchemaJSON: BusRouteListInputSchemaJSON, SideEffect: capability.SideEffectRead,
		},
		{
			ID: BusJourneySearchToolID, Version: PackageVersion,
			Description:     "Search authoritative campus bus journeys by stops and departure time.",
			InputSchemaJSON: BusJourneySearchInputSchemaJSON, SideEffect: capability.SideEffectRead,
		},
	}
}

// ServiceSpec 返回 campus 服务规格。
func ServiceSpec() capability.ServiceSpec {
	return capability.ServiceSpec{
		ID: ServiceID, Version: PackageVersion,
		Description:      "Campus-wide public services.",
		ToolDependencies: []string{BusStopSearchToolID, BusRouteListToolID, BusJourneySearchToolID},
	}
}

// CapabilitySpecs 返回 campus 对外暴露的 Capability 规格。
func CapabilitySpecs() []capability.CapabilitySpec {
	return []capability.CapabilitySpec{
		{
			ID: BusStopSearchCapabilityID, Version: PackageVersion, Name: "查询校巴站点",
			Description: "Search campus bus stops by a user-provided stop name or alias.",
			ServiceID:   ServiceID, InputSchemaJSON: BusStopSearchInputSchemaJSON,
			SideEffect: capability.SideEffectRead, ToolID: BusStopSearchToolID,
		},
		{
			ID: BusRouteListCapabilityID, Version: PackageVersion, Name: "列出校巴线路",
			Description: "List currently available campus bus routes and directions.",
			ServiceID:   ServiceID, InputSchemaJSON: BusRouteListInputSchemaJSON,
			SideEffect: capability.SideEffectRead, ToolID: BusRouteListToolID,
		},
		{
			ID: BusJourneySearchCapabilityID, Version: PackageVersion, Name: "查询校巴行程",
			Description: "Find direct campus bus journeys for stable origin and destination stop IDs and a departure time.",
			ServiceID:   ServiceID, InputSchemaJSON: BusJourneySearchInputSchemaJSON,
			SideEffect: capability.SideEffectRead, ToolID: BusJourneySearchToolID,
		},
	}
}

// PackageVersion 是测试/部署夹具使用的包版本（与包仓库 ailuo.toml 一致）。
const PackageVersion = "1.0.0"

// Extensions 返回 campus 的 extensions 段（tools/service/capabilities JSON），
// 供 campustest 装配与 SDK 生成测试共用同一契约副本。
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
