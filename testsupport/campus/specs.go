package campus

import (
	"encoding/json"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

// 校巴测试夹具使用的 App、包和组件标识。业务实现位于 packages/campus-bus；
// 本目录只供测试装配与 SDK 生成测试使用。
const (
	AppID            = "campus-services"
	PackageID        = "campus"
	BusComponentID   = "bus"
	StorageNamespace = "campus/bus"

	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
)

const (
	BusStopSearchInputSchemaJSON    = `{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["query"],"additionalProperties":false}`
	BusRouteListInputSchemaJSON     = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	BusJourneySearchInputSchemaJSON = `{"type":"object","properties":{"origin_stop_id":{"type":"string","minLength":1},"destination_stop_id":{"type":"string","minLength":1},"depart_after":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["origin_stop_id","destination_stop_id"],"additionalProperties":false}`
	PackageVersion                  = "1.0.0"
)

// Capabilities 返回校巴对外暴露的 Capability 规格。
func Capabilities() []capability.CapabilitySpec {
	return []capability.CapabilitySpec{
		{
			ID: BusStopSearchCapabilityID, Version: PackageVersion, Name: "查询校巴站点",
			Description:     "Search campus bus stops by a user-provided stop name or alias.",
			InputSchemaJSON: BusStopSearchInputSchemaJSON,
			Authorization:   capability.AuthorizationSpec{ResourceType: "campus.bus.catalog"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
		{
			ID: BusRouteListCapabilityID, Version: PackageVersion, Name: "列出校巴线路",
			Description:     "List currently available campus bus routes and directions.",
			InputSchemaJSON: BusRouteListInputSchemaJSON,
			Authorization:   capability.AuthorizationSpec{ResourceType: "campus.bus.catalog"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
		{
			ID: BusJourneySearchCapabilityID, Version: PackageVersion, Name: "查询校巴行程",
			Description:     "Find direct campus bus journeys for stable origin and destination stop IDs and a departure time.",
			InputSchemaJSON: BusJourneySearchInputSchemaJSON,
			Authorization:   capability.AuthorizationSpec{ResourceType: "campus.bus.network"},
			Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		},
	}
}

// CapabilitiesJSON 返回供 SDK 生成测试使用的 Capability JSON。
func CapabilitiesJSON() (json.RawMessage, error) {
	return json.Marshal(Capabilities())
}
