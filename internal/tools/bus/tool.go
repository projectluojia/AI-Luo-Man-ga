package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	StopSearchToolID       = "campus.bus.stops.search"
	RouteListToolID        = "campus.bus.routes.list"
	JourneySearchToolID    = "campus.bus.schedule.search"
	RealtimePositionToolID = "campus.bus.vehicles.realtime"

	StopSearchInputSchemaJSON       = `{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["query"],"additionalProperties":false}`
	RouteListInputSchemaJSON        = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	JourneySearchInputSchemaJSON    = `{"type":"object","properties":{"origin_stop_id":{"type":"string","minLength":1},"destination_stop_id":{"type":"string","minLength":1},"depart_after":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["origin_stop_id","destination_stop_id"],"additionalProperties":false}`
	RealtimePositionInputSchemaJSON = `{"type":"object","properties":{"route_id":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`
)

// ToolSpecs 返回校巴工具规格（单一来源，供安装清单与 Registry 注册共用）。
func ToolSpecs() []registry.ToolSpec {
	return []registry.ToolSpec{
		{
			ID:              StopSearchToolID,
			Version:         "1.0.0",
			Description:     "Search authoritative campus bus stops by name or alias.",
			InputSchemaJSON: StopSearchInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
		{ID: RealtimePositionToolID, Version: "1.0.0", Description: "Query authorized real-time campus bus vehicle positions.", InputSchemaJSON: RealtimePositionInputSchemaJSON, SideEffect: registry.SideEffectRead},
		{
			ID:              RouteListToolID,
			Version:         "1.0.0",
			Description:     "List authoritative campus bus routes.",
			InputSchemaJSON: RouteListInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
		{
			ID:              JourneySearchToolID,
			Version:         "1.0.0",
			Description:     "Search authoritative campus bus journeys by stops and departure time.",
			InputSchemaJSON: JourneySearchInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
	}
}

// ToolHandlers 返回校巴工具执行器（注入存储端口），供 hosted 包 guest 装配使用。
func ToolHandlers(store Store) map[string]registry.Handler {
	return map[string]registry.Handler{
		StopSearchToolID:       stopSearchHandler(store),
		RouteListToolID:        routeListHandler(store),
		JourneySearchToolID:    journeySearchHandler(store, time.Now),
		RealtimePositionToolID: realtimePositionHandler(store, time.Now),
	}
}

// realtimePositionHandler 在校方授权、权威且新鲜的快照上执行实时位置查询。
func realtimePositionHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, requestContext contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		var request RealtimePositionRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, fmt.Errorf("decode bus realtime position request: %w", err)
		}
		if err := request.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		snapshot, err := store.SearchVehiclePositions(ctx, requestContext.AppID, request)
		if err != nil {
			return nil, fmt.Errorf("search bus vehicle positions: %w", err)
		}
		status, err := snapshot.Metadata.GovernRealtime(now())
		if err != nil {
			if errors.Is(err, contracts.ErrRealtimeUnauthorized) {
				observe.Warn(ctx, "校巴实时位置因未获授权拒绝")
			} else {
				logRejectedSnapshot(ctx, "vehicle_positions", snapshot.Metadata, err)
			}
			return nil, fmt.Errorf("govern bus realtime positions: %w", err)
		}
		positions := snapshot.Positions
		for _, position := range positions {
			if position.SourceRevision != snapshot.Metadata.Revision || position.RecordedAt.IsZero() || position.Latitude < -90 || position.Latitude > 90 || position.Longitude < -180 || position.Longitude > 180 {
				return nil, fmt.Errorf("bus vehicle position validation: %w", contracts.ErrDataIncomplete)
			}
		}
		if positions == nil {
			positions = []VehiclePosition{}
		}
		return marshalResult(RealtimePositionResult{DataStatus: status, Positions: positions})
	}
}

// governedStatus 治理一次校巴快照读取：快照元数据未通过权威/新鲜度校验时
// 记录拒绝日志并返回稳定数据治理错误。
func governedStatus(ctx context.Context, kind string, metadata SnapshotMetadata, now time.Time) (DataStatus, error) {
	dataStatus, err := metadata.Govern(now)
	if err != nil {
		logRejectedSnapshot(ctx, kind, metadata, err)
		return DataStatus{}, err
	}
	return dataStatus, nil
}

func stopSearchHandler(store Store) registry.Handler {
	return func(ctx context.Context, requestContext contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		var request StopSearchRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, fmt.Errorf("decode bus stop search request: %w", err)
		}
		if err := request.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始查询校巴站点",
			observe.StringAttr("app_id", requestContext.AppID),
			observe.IntAttr("query_length", utf8.RuneCountInString(request.Query)),
			observe.IntAttr("limit", request.Limit),
		)
		snapshot, err := store.SearchStops(ctx, requestContext.AppID, request)
		if err != nil {
			return nil, fmt.Errorf("search bus stops: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "stops", snapshot.Metadata, time.Now())
		if err != nil {
			return nil, fmt.Errorf("govern bus stop snapshot: %w", err)
		}
		stops := snapshot.Stops
		for _, stop := range stops {
			if stop.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus stop snapshot revision mismatch: %w", contracts.ErrDataIncomplete)
			}
		}
		if stops == nil {
			stops = []Stop{}
		}
		observe.Info(ctx, "校巴站点查询完成",
			observe.IntAttr("result_count", len(stops)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.StringAttr("valid_until", dataStatus.ValidUntil.Format(time.RFC3339)),
			observe.Duration(started),
		)
		return marshalResult(StopSearchResult{DataStatus: dataStatus, Stops: stops})
	}
}

func routeListHandler(store Store) registry.Handler {
	return func(ctx context.Context, requestContext contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		var request RouteListRequest
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &request); err != nil {
				return nil, fmt.Errorf("decode bus route list request: %w", err)
			}
		}
		if err := request.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始查询校巴线路",
			observe.StringAttr("app_id", requestContext.AppID),
			observe.IntAttr("limit", request.Limit),
		)
		snapshot, err := store.ListRoutes(ctx, requestContext.AppID, request)
		if err != nil {
			return nil, fmt.Errorf("list bus routes: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "routes", snapshot.Metadata, time.Now())
		if err != nil {
			return nil, fmt.Errorf("govern bus route snapshot: %w", err)
		}
		routes := snapshot.Routes
		for _, route := range routes {
			if route.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus route snapshot revision mismatch: %w", contracts.ErrDataIncomplete)
			}
		}
		if routes == nil {
			routes = []Route{}
		}
		observe.Info(ctx, "校巴线路查询完成",
			observe.IntAttr("result_count", len(routes)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.StringAttr("valid_until", dataStatus.ValidUntil.Format(time.RFC3339)),
			observe.Duration(started),
		)
		return marshalResult(RouteListResult{DataStatus: dataStatus, Routes: routes})
	}
}

func journeySearchHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, requestContext contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		var request SearchRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, fmt.Errorf("decode bus journey search request: %w", err)
		}
		if err := request.NormalizeAndValidate(now()); err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始查询校巴班次",
			observe.StringAttr("app_id", requestContext.AppID),
			observe.StringAttr("origin_stop_id", request.OriginStopID),
			observe.StringAttr("destination_stop_id", request.DestinationStopID),
			observe.StringAttr("depart_after", request.DepartAfter.Format(time.RFC3339)),
			observe.IntAttr("limit", request.Limit),
		)
		snapshot, err := store.SearchJourneys(ctx, requestContext.AppID, request)
		if err != nil {
			return nil, fmt.Errorf("search bus journeys: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "journeys", snapshot.Metadata, now())
		if err != nil {
			return nil, fmt.Errorf("govern bus journey snapshot: %w", err)
		}
		journeys := snapshot.Journeys
		for _, journey := range journeys {
			if journey.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus journey snapshot revision mismatch: %w", contracts.ErrDataIncomplete)
			}
		}
		if journeys == nil {
			journeys = []Journey{}
		}
		observe.Info(ctx, "校巴班次查询完成",
			observe.IntAttr("result_count", len(journeys)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.StringAttr("valid_until", dataStatus.ValidUntil.Format(time.RFC3339)),
			observe.Duration(started),
		)
		return marshalResult(SearchResult{DataStatus: dataStatus, Journeys: journeys})
	}
}

func marshalResult(value any) (json.RawMessage, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode bus tool result: %w", err)
	}
	return result, nil
}

func logRejectedSnapshot(ctx context.Context, dataKind string, metadata SnapshotMetadata, err error) {
	errorCode := "data_incomplete"
	switch {
	case errors.Is(err, contracts.ErrDataUntrusted):
		errorCode = "data_non_authoritative"
	case errors.Is(err, contracts.ErrDataExpired):
		errorCode = "data_expired"
	case errors.Is(err, contracts.ErrRealtimeUnauthorized):
		errorCode = "realtime_unauthorized"
	}
	observe.Warn(ctx, "校巴数据快照未通过使用治理",
		observe.StringAttr("data_kind", dataKind),
		observe.StringAttr("source_revision", metadata.Revision),
		observe.BoolAttr("authoritative", metadata.Authoritative),
		observe.BoolAttr("complete", metadata.Complete),
		observe.StringAttr("error_code", errorCode),
	)
}
