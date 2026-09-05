package campus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
)

// busHostQuery 是 guest 存储查询投影的载荷信封：操作标识 + 参数。
type busHostQuery struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

// busHostResponse 是宿主函数返回给 guest 的信封：成功携带结果，失败携带内部错误码。
type busHostResponse struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// HostedFunctions 返回 campus hosted 包所需的宿主函数：把权威存储查询投影给沙箱 guest。
// App 隔离以宿主侧治理上下文为准，guest 无法伪造；数据新鲜度/权威性治理在 guest 侧完成。
func HostedFunctions(store bus.Store) []loader.HostedFunction {
	return []loader.HostedFunction{{
		Module: "ailuo.bus",
		Name:   "query",
		Call:   busQueryHandler(store),
	}}
}

// busQueryHandler 处理 guest 的存储查询投影。
// 协议违例（坏载荷、未知操作）返回错误令 guest 得到 -1；存储失败返回内部错误信封。
func busQueryHandler(store bus.Store) func(context.Context, contracts.RequestContext, []byte) ([]byte, error) {
	return func(ctx context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
		var query busHostQuery
		if err := json.Unmarshal(body, &query); err != nil {
			return nil, fmt.Errorf("decode bus host query: %w", err)
		}
		var result any
		var queryErr error
		switch query.Op {
		case "search_stops":
			var args bus.StopSearchRequest
			if err := json.Unmarshal(query.Args, &args); err != nil {
				return nil, fmt.Errorf("decode stop search args: %w", err)
			}
			result, queryErr = store.SearchStops(ctx, request.AppID, args)
		case "list_routes":
			var args bus.RouteListRequest
			if err := json.Unmarshal(query.Args, &args); err != nil {
				return nil, fmt.Errorf("decode route list args: %w", err)
			}
			result, queryErr = store.ListRoutes(ctx, request.AppID, args)
		case "search_journeys":
			var args bus.SearchRequest
			if err := json.Unmarshal(query.Args, &args); err != nil {
				return nil, fmt.Errorf("decode journey search args: %w", err)
			}
			result, queryErr = store.SearchJourneys(ctx, request.AppID, args)
		default:
			return nil, fmt.Errorf("unknown bus query op: %s", query.Op)
		}
		if queryErr != nil {
			return json.Marshal(busHostResponse{OK: false, Code: busHostErrorCode(queryErr), Message: queryErr.Error()})
		}
		resultBytes, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("marshal bus host query result: %w", err)
		}
		return json.Marshal(busHostResponse{OK: true, Result: resultBytes})
	}
}

// busHostErrorCode 把存储层错误映射为信封闭式错误码，保留数据治理类别
// （如跨 App 无数据），其余存储故障归为内部错误。
func busHostErrorCode(err error) string {
	switch {
	case errors.Is(err, bus.ErrDataUnavailable):
		return "data_unavailable"
	case errors.Is(err, bus.ErrDataIncomplete):
		return "data_incomplete"
	case errors.Is(err, bus.ErrDataUntrusted):
		return "data_untrusted"
	case errors.Is(err, bus.ErrDataExpired):
		return "data_expired"
	default:
		return "internal"
	}
}
