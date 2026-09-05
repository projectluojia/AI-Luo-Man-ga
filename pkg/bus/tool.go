package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	StopSearchToolID    = "campus.bus.stops.search"
	RouteListToolID     = "campus.bus.routes.list"
	JourneySearchToolID = "campus.bus.journeys.search"

	StopSearchInputSchemaJSON    = `{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["query"],"additionalProperties":false}`
	RouteListInputSchemaJSON     = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	JourneySearchInputSchemaJSON = `{"type":"object","properties":{"origin_stop_id":{"type":"string","minLength":1},"destination_stop_id":{"type":"string","minLength":1},"depart_after":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["origin_stop_id","destination_stop_id"],"additionalProperties":false}`
)

// Handler 是校巴工具执行器：以治理上下文执行一次查询。AppID 由调用方经
// 宿主侧注入（guest 场景传空字符串，宿主侧强制隔离）。本包不感知内核治理
// 上下文（contracts.RequestContext），App 隔离是调用方职责。
type Handler func(context.Context, string, json.RawMessage) (json.RawMessage, error)

// ToolHandlers 返回校巴工具执行器（注入存储端口），供 hosted 包 guest 装配使用。
func ToolHandlers(store Store) map[string]Handler {
	return map[string]Handler{
		StopSearchToolID:    stopSearchHandler(store),
		RouteListToolID:     routeListHandler(store),
		JourneySearchToolID: journeySearchHandler(store, time.Now),
	}
}

// governedStatus 治理一次校巴快照读取：快照元数据未通过权威/新鲜度校验时
// 返回稳定数据治理错误（拒绝原因在错误链中，不写日志——本包中立无日志依赖）。
func governedStatus(ctx context.Context, kind string, metadata SnapshotMetadata, now time.Time) (DataStatus, error) {
	dataStatus, err := metadata.Govern(now)
	if err != nil {
		return DataStatus{}, fmt.Errorf("govern %s snapshot: %w", kind, err)
	}
	return dataStatus, nil
}

func stopSearchHandler(store Store) Handler {
	return func(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
		var request StopSearchRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, fmt.Errorf("decode bus stop search request: %w", err)
		}
		if err := request.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		snapshot, err := store.SearchStops(ctx, appID, request)
		if err != nil {
			return nil, fmt.Errorf("search bus stops: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "stops", snapshot.Metadata, time.Now())
		if err != nil {
			return nil, err
		}
		stops := snapshot.Stops
		for _, stop := range stops {
			if stop.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus stop snapshot revision mismatch: %w", ErrDataIncomplete)
			}
		}
		if stops == nil {
			stops = []Stop{}
		}
		return marshalResult(StopSearchResult{DataStatus: dataStatus, Stops: stops})
	}
}

func routeListHandler(store Store) Handler {
	return func(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
		var request RouteListRequest
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &request); err != nil {
				return nil, fmt.Errorf("decode bus route list request: %w", err)
			}
		}
		if err := request.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		snapshot, err := store.ListRoutes(ctx, appID, request)
		if err != nil {
			return nil, fmt.Errorf("list bus routes: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "routes", snapshot.Metadata, time.Now())
		if err != nil {
			return nil, err
		}
		routes := snapshot.Routes
		for _, route := range routes {
			if route.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus route snapshot revision mismatch: %w", ErrDataIncomplete)
			}
		}
		if routes == nil {
			routes = []Route{}
		}
		return marshalResult(RouteListResult{DataStatus: dataStatus, Routes: routes})
	}
}

func journeySearchHandler(store Store, now func() time.Time) Handler {
	return func(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
		var request SearchRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, fmt.Errorf("decode bus journey search request: %w", err)
		}
		if err := request.NormalizeAndValidate(now()); err != nil {
			return nil, err
		}
		snapshot, err := store.SearchJourneys(ctx, appID, request)
		if err != nil {
			return nil, fmt.Errorf("search bus journeys: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "journeys", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		journeys := snapshot.Journeys
		for _, journey := range journeys {
			if journey.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("bus journey snapshot revision mismatch: %w", ErrDataIncomplete)
			}
		}
		if journeys == nil {
			journeys = []Journey{}
		}
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
