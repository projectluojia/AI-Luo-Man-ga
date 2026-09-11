// Package bus 是校巴包的领域层：4 个 Capability 的载荷校验、集合读取与
// 结果组装。本层不接触 wasmimport——宿主函数访问经注入的 lister 接口完成，
// 因此可以在原生 go test 下完整测试业务逻辑与数据治理。
//
// 数据全部来自系统作用域快照集合（stops/routes/journeys/vehicle_positions），
// 由可信集成方经快照导入写入；vehicle_positions 仅在获得校方授权时导入，
// 未授权与暂无位置对调用方表现一致（空结果），不泄漏授权状态。
package bus

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/guestkit"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	StopSearchCapabilityID       = "campus.bus.stops.search"
	RouteListCapabilityID        = "campus.bus.routes.list"
	JourneySearchCapabilityID    = "campus.bus.journeys.search"
	RealtimePositionCapabilityID = "campus.bus.vehicles.realtime"
)

// 快照集合名（namespace campus/bus 下的集合）。
const (
	stopsCollection            = "stops"
	routesCollection           = "routes"
	journeysCollection         = "journeys"
	vehiclePositionsCollection = "vehicle_positions"
)

// 载荷上限（与各 Capability schema 一一对应）。
const (
	defaultListLimit    = 10
	defaultRoutesLimit  = 50
	defaultRealtimeSize = 50
	maxListLimit        = 50
	maxRealtimeLimit    = 100
)

// Stop 是站点文档（campus/bus 的 stops 集合）。
type Stop struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Aliases        []string `json:"aliases,omitempty"`
	Latitude       float64  `json:"latitude,omitempty"`
	Longitude      float64  `json:"longitude,omitempty"`
	SourceRevision string   `json:"source_revision"`
}

type StopSearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type StopSearchResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Stops      []Stop              `json:"stops"`
}

// Route 是线路文档（campus/bus 的 routes 集合）。
type Route struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Direction         string `json:"direction"`
	OriginStopID      string `json:"origin_stop_id"`
	DestinationStopID string `json:"destination_stop_id"`
	SourceRevision    string `json:"source_revision"`
}

type RouteListRequest struct {
	Limit int `json:"limit"`
}

type RouteListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Routes     []Route             `json:"routes"`
}

// Journey 是行程文档（campus/bus 的 journeys 集合）。
type Journey struct {
	TripID            string    `json:"trip_id"`
	RouteID           string    `json:"route_id"`
	RouteName         string    `json:"route_name"`
	Direction         string    `json:"direction"`
	OriginStopID      string    `json:"origin_stop_id"`
	OriginStopName    string    `json:"origin_stop_name"`
	DestinationStopID string    `json:"destination_stop_id"`
	DestinationName   string    `json:"destination_stop_name"`
	DepartureAt       time.Time `json:"departure_at"`
	ArrivalAt         time.Time `json:"arrival_at"`
	SourceRevision    string    `json:"source_revision"`
}

type JourneySearchRequest struct {
	OriginStopID      string    `json:"origin_stop_id"`
	DestinationStopID string    `json:"destination_stop_id"`
	DepartAfter       time.Time `json:"depart_after"`
	Limit             int       `json:"limit"`
}

type JourneySearchResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Journeys   []Journey           `json:"journeys"`
}

// VehiclePosition 是实时车辆位置文档（vehicle_positions 集合，仅授权后导入）。
type VehiclePosition struct {
	VehicleID      string    `json:"vehicle_id"`
	RouteID        string    `json:"route_id"`
	Latitude       float64   `json:"latitude"`
	Longitude      float64   `json:"longitude"`
	RecordedAt     time.Time `json:"recorded_at"`
	SourceRevision string    `json:"source_revision"`
}

type RealtimePositionRequest struct {
	RouteID string `json:"route_id"`
	Limit   int    `json:"limit"`
}

type RealtimePositionResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Positions  []VehiclePosition   `json:"positions"`
}

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		StopSearchCapabilityID:       func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.searchStops) },
		RouteListCapabilityID:        func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listRoutes) },
		JourneySearchCapabilityID:    func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.searchJourneys) },
		RealtimePositionCapabilityID: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.realtimePositions) },
	}
}

var errInvalidArgument = guestkit.ErrInvalidArgument

// searchStops 处理站点搜索：按名称或别名匹配，按名称排序。
func (d *dispatcher) searchStops(request StopSearchRequest) (any, error) {
	query := strings.TrimSpace(request.Query)
	if request.Limit == 0 {
		request.Limit = defaultListLimit
	}
	if query == "" || request.Limit < 1 || request.Limit > maxListLimit {
		return nil, errInvalidArgument
	}
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[Stop](d.store, stopsCollection)
	if err != nil {
		return nil, err
	}
	matches := make([]Stop, 0, len(snapshot.Documents))
	needle := strings.ToLower(query)
	for _, item := range snapshot.Documents {
		if matchesStop(item, needle) {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	return StopSearchResult{DataStatus: dataStatus, Stops: matches}, nil
}

// listRoutes 处理线路列表：按名称、方向排序。
func (d *dispatcher) listRoutes(request RouteListRequest) (any, error) {
	if request.Limit == 0 {
		request.Limit = defaultRoutesLimit
	}
	if request.Limit < 1 || request.Limit > maxListLimit {
		return nil, errInvalidArgument
	}
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[Route](d.store, routesCollection)
	if err != nil {
		return nil, err
	}
	routes := make([]Route, 0, len(snapshot.Documents))
	routes = append(routes, snapshot.Documents...)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Name != routes[j].Name {
			return routes[i].Name < routes[j].Name
		}
		return routes[i].Direction < routes[j].Direction
	})
	if len(routes) > request.Limit {
		routes = routes[:request.Limit]
	}
	return RouteListResult{DataStatus: dataStatus, Routes: routes}, nil
}

// searchJourneys 处理行程查询：按起止站与出发时间过滤，按出发时间排序。
func (d *dispatcher) searchJourneys(request JourneySearchRequest) (any, error) {
	if request.Limit == 0 {
		request.Limit = defaultListLimit
	}
	if request.OriginStopID == "" || request.DestinationStopID == "" ||
		request.OriginStopID == request.DestinationStopID || request.Limit < 1 || request.Limit > maxListLimit {
		return nil, errInvalidArgument
	}
	if request.DepartAfter.IsZero() {
		request.DepartAfter = time.Now()
	}
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[Journey](d.store, journeysCollection)
	if err != nil {
		return nil, err
	}
	matches := make([]Journey, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if item.OriginStopID == request.OriginStopID && item.DestinationStopID == request.DestinationStopID &&
			!item.DepartureAt.Before(request.DepartAfter) {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].DepartureAt.Before(matches[j].DepartureAt) })
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	return JourneySearchResult{DataStatus: dataStatus, Journeys: matches}, nil
}

// realtimePositions 处理实时位置查询：vehicle_positions 集合仅在校方授权后
// 由可信集成方导入，未授权与暂无上报都表现为权威快照下的空位置列表。
// 坐标与时标逐项校验，脏数据整体拒绝。
func (d *dispatcher) realtimePositions(request RealtimePositionRequest) (any, error) {
	if request.Limit == 0 {
		request.Limit = defaultRealtimeSize
	}
	if request.Limit < 1 || request.Limit > maxRealtimeLimit {
		return nil, errInvalidArgument
	}
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[VehiclePosition](d.store, vehiclePositionsCollection)
	if err != nil {
		return nil, err
	}
	// 快照含每车历史轨迹：先过滤+逐项校验，再按车辆取最新上报，
	// 最后按时间降序输出（最新在前）。脏数据仍整体拒绝。
	latest := make(map[string]VehiclePosition, len(snapshot.Documents))
	order := make([]string, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if request.RouteID != "" && item.RouteID != request.RouteID {
			continue
		}
		if item.RecordedAt.IsZero() || item.Latitude < -90 || item.Latitude > 90 || item.Longitude < -180 || item.Longitude > 180 {
			return nil, guestkit.ErrDataIncomplete
		}
		if prev, ok := latest[item.VehicleID]; !ok {
			order = append(order, item.VehicleID)
			latest[item.VehicleID] = item
		} else if item.RecordedAt.After(prev.RecordedAt) {
			latest[item.VehicleID] = item
		}
	}
	positions := make([]VehiclePosition, 0, len(order))
	for _, vehicleID := range order {
		positions = append(positions, latest[vehicleID])
	}
	sort.SliceStable(positions, func(i, j int) bool {
		return positions[i].RecordedAt.After(positions[j].RecordedAt)
	})
	if len(positions) > request.Limit {
		positions = positions[:request.Limit]
	}
	return RealtimePositionResult{DataStatus: dataStatus, Positions: positions}, nil
}

func matchesStop(item Stop, needle string) bool {
	if strings.Contains(strings.ToLower(item.Name), needle) {
		return true
	}
	for _, alias := range item.Aliases {
		if strings.Contains(strings.ToLower(alias), needle) {
			return true
		}
	}
	return false
}
