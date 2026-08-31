//go:build wasip1

// campus 是校园服务的 hosted 包 guest：实现校园三工具（站点搜索/线路列表/
// 行程查询）。业务数据经通用 ailuo.store 宿主函数读取宿主托管的权威存储
// （App 隔离由宿主治理上下文强制，namespace 绑定为清单 [storage] 声明值，
// guest 无法选择作用域）。数据新鲜度/权威性治理在本侧完成：快照元数据
// 不完整、非权威或过期时返回稳定数据治理错误码。本包只依赖标准库。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unsafe"
)

//go:wasmimport ailuo.store list
func storeList(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

const (
	// hostFunctionError 是宿主函数调用失败的长度标记（-1 的无符号表示）。
	hostFunctionError = 0xFFFFFFFF
	// maxStoreResponse 是宿主函数响应缓冲上限（与内核消息上限一致）。
	maxStoreResponse = 512 << 10

	routesCollection   = "routes"
	stopsCollection    = "stops"
	journeysCollection = "journeys"

	stopsToolID    = "campus.bus.stops.search"
	routesToolID   = "campus.bus.routes.list"
	journeysToolID = "campus.bus.journeys.search"
)

// 稳定错误码（内核闭式集合：guest 自定义码会被内核视为协议违例）。
const (
	codeInvalidArgument = "invalid_argument"
	codeInternal        = "internal"
	codeDataUnavailable = "data_unavailable"
	codeDataIncomplete  = "data_incomplete"
	codeDataUntrusted   = "data_untrusted"
	codeDataExpired     = "data_expired"
)

// requestEnvelope 与宿主约定的 stdin 调用信封。
type requestEnvelope struct {
	ToolID  string          `json:"tool_id"`
	Payload json.RawMessage `json:"payload"`
}

// resultEnvelope 与宿主约定的 stdout 结果信封。
type resultEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// stop 是站点文档（campus/bus 的 stops 集合）。
type stop struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Aliases        []string `json:"aliases,omitempty"`
	Latitude       float64  `json:"latitude,omitempty"`
	Longitude      float64  `json:"longitude,omitempty"`
	SourceRevision string   `json:"source_revision"`
}

type stopSearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type stopSearchResult struct {
	DataStatus dataStatus `json:"data_status"`
	Stops      []stop     `json:"stops"`
}

// route 是线路文档（campus/bus 的 routes 集合）。
type route struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Direction         string `json:"direction"`
	OriginStopID      string `json:"origin_stop_id"`
	DestinationStopID string `json:"destination_stop_id"`
	SourceRevision    string `json:"source_revision"`
}

type routeListRequest struct {
	Limit int `json:"limit"`
}

type routeListResult struct {
	DataStatus dataStatus `json:"data_status"`
	Routes     []route    `json:"routes"`
}

// journey 是行程文档（campus/bus 的 journeys 集合）。
type journey struct {
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

type journeySearchRequest struct {
	OriginStopID      string    `json:"origin_stop_id"`
	DestinationStopID string    `json:"destination_stop_id"`
	DepartAfter       time.Time `json:"depart_after"`
	Limit             int       `json:"limit"`
}

type journeySearchResult struct {
	DataStatus dataStatus `json:"data_status"`
	Journeys   []journey  `json:"journeys"`
}

// dataStatus 是工具结果的治理状态（快照元数据扁平展开）。
type dataStatus struct {
	State          string    `json:"state"`
	SourceRevision string    `json:"source_revision"`
	Source         string    `json:"source"`
	Authoritative  bool      `json:"authoritative"`
	Complete       bool      `json:"complete"`
	ImportedAt     time.Time `json:"imported_at"`
	ValidUntil     time.Time `json:"valid_until"`
}

// ailuo.store list 的请求与响应信封。Meta 是与本次读取一致的快照元数据
// （宿主单事务读出）：guest 据此做新鲜度/权威性治理，并发快照替换下不会
// 观察到跨修订混合。
type storeListRequest struct {
	Collection string `json:"collection"`
	Limit      int    `json:"limit"`
	AfterID    string `json:"after_id,omitempty"`
}

type storeDocument struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"doc"`
}

type storeListResponse struct {
	Docs      []storeDocument `json:"docs"`
	Meta      snapshotMeta    `json:"meta"`
	MetaFound bool            `json:"meta_found"`
}

type snapshotMeta struct {
	SourceRevision string    `json:"source_revision"`
	Source         string    `json:"source"`
	Authoritative  bool      `json:"authoritative"`
	Complete       bool      `json:"complete"`
	ImportedAt     time.Time `json:"imported_at"`
	ValidUntil     time.Time `json:"valid_until"`
}

// collectionSnapshot 是一次一致的集合读取：文档与快照元数据同源。
type collectionSnapshot[T any] struct {
	Meta      snapshotMeta
	MetaFound bool
	Documents []T
}

func main() {
	// 无日志初始化：wasm guest 的 stdout 只承载结果信封，日志由宿主侧丢弃。
	run(os.Stdin, os.Stdout)
}

func run(input io.Reader, output io.Writer) {
	inputBytes, err := io.ReadAll(input)
	if err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInternal, Message: "read stdin failed"})
		return
	}
	var request requestEnvelope
	if err := json.Unmarshal(inputBytes, &request); err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "request envelope is malformed"})
		return
	}
	// 按工具分发：工具标识来自宿主治理上下文，schema 由清单声明约束。
	switch request.ToolID {
	case stopsToolID:
		invokeStopSearch(output, request.Payload)
	case routesToolID:
		invokeRouteList(output, request.Payload)
	case journeysToolID:
		invokeJourneySearch(output, request.Payload)
	default:
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "unknown tool: " + request.ToolID})
	}
}

// invokeStopSearch 处理站点搜索：按名称或别名匹配，按名称排序。
func invokeStopSearch(output io.Writer, payload json.RawMessage) {
	var request stopSearchRequest
	if err := decodeRequest(payload, &request); err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: err.Error()})
		return
	}
	if request.Limit == 0 {
		request.Limit = 10
	}
	if strings.TrimSpace(request.Query) == "" || request.Limit < 1 || request.Limit > 50 {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "query is required and limit must be between 1 and 50"})
		return
	}
	snapshot, err := fetchCollection[stop](stopsCollection)
	if err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInternal, Message: err.Error()})
		return
	}
	status, statusErr := governedStatus(snapshot)
	if statusErr != nil {
		writeEnvelope(output, resultEnvelope{Code: statusErr.Error(), Message: "snapshot failed governance"})
		return
	}
	matches := make([]stop, 0, len(snapshot.Documents))
	query := strings.ToLower(strings.TrimSpace(request.Query))
	for _, item := range snapshot.Documents {
		if item.SourceRevision == status.SourceRevision && matchesStop(item, query) {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	writeResult(output, stopSearchResult{DataStatus: status, Stops: matches})
}

// invokeRouteList 处理线路列表：按名称、方向排序。
func invokeRouteList(output io.Writer, payload json.RawMessage) {
	var request routeListRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &request); err != nil {
			writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "payload is malformed"})
			return
		}
	}
	if request.Limit == 0 {
		request.Limit = 50
	}
	if request.Limit < 1 || request.Limit > 50 {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "limit must be between 1 and 50"})
		return
	}
	snapshot, err := fetchCollection[route](routesCollection)
	if err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInternal, Message: err.Error()})
		return
	}
	status, statusErr := governedStatus(snapshot)
	if statusErr != nil {
		writeEnvelope(output, resultEnvelope{Code: statusErr.Error(), Message: "snapshot failed governance"})
		return
	}
	routes := make([]route, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if item.SourceRevision == status.SourceRevision {
			routes = append(routes, item)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Name != routes[j].Name {
			return routes[i].Name < routes[j].Name
		}
		return routes[i].Direction < routes[j].Direction
	})
	if len(routes) > request.Limit {
		routes = routes[:request.Limit]
	}
	writeResult(output, routeListResult{DataStatus: status, Routes: routes})
}

// invokeJourneySearch 处理行程查询：按起止站与出发时间过滤，按出发时间排序。
func invokeJourneySearch(output io.Writer, payload json.RawMessage) {
	var request journeySearchRequest
	if err := decodeRequest(payload, &request); err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: err.Error()})
		return
	}
	if request.Limit == 0 {
		request.Limit = 10
	}
	if request.OriginStopID == "" || request.DestinationStopID == "" ||
		request.OriginStopID == request.DestinationStopID || request.Limit < 1 || request.Limit > 50 {
		writeEnvelope(output, resultEnvelope{Code: codeInvalidArgument, Message: "origin and destination stops are required and must differ; limit must be between 1 and 50"})
		return
	}
	if request.DepartAfter.IsZero() {
		request.DepartAfter = time.Now()
	}
	snapshot, err := fetchCollection[journey](journeysCollection)
	if err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInternal, Message: err.Error()})
		return
	}
	status, statusErr := governedStatus(snapshot)
	if statusErr != nil {
		writeEnvelope(output, resultEnvelope{Code: statusErr.Error(), Message: "snapshot failed governance"})
		return
	}
	matches := make([]journey, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if item.SourceRevision == status.SourceRevision &&
			item.OriginStopID == request.OriginStopID && item.DestinationStopID == request.DestinationStopID &&
			!item.DepartureAt.Before(request.DepartAfter) {
			matches = append(matches, item)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].DepartureAt.Before(matches[j].DepartureAt) })
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	writeResult(output, journeySearchResult{DataStatus: status, Journeys: matches})
}

// govern 执行数据新鲜度/权威性治理：失败返回稳定错误码。
func govern(meta snapshotMeta) (dataStatus, error) {
	if meta.SourceRevision == "" || meta.Source == "" || !meta.Complete ||
		meta.ImportedAt.IsZero() || meta.ValidUntil.IsZero() ||
		!meta.ValidUntil.After(meta.ImportedAt) || time.Now().Before(meta.ImportedAt) {
		return dataStatus{}, errors.New(codeDataIncomplete)
	}
	if !meta.Authoritative {
		return dataStatus{}, errors.New(codeDataUntrusted)
	}
	if !time.Now().Before(meta.ValidUntil) {
		return dataStatus{}, errors.New(codeDataExpired)
	}
	return dataStatus{
		State: "authoritative_fresh", SourceRevision: meta.SourceRevision, Source: meta.Source,
		Authoritative: true, Complete: true, ImportedAt: meta.ImportedAt, ValidUntil: meta.ValidUntil,
	}, nil
}

// governedStatus 先治理元数据再返回状态；文档修订一致性由调用方逐项校验。
func governedStatus[T any](snapshot collectionSnapshot[T]) (dataStatus, error) {
	if !snapshot.MetaFound {
		return dataStatus{}, errors.New(codeDataUnavailable)
	}
	return govern(snapshot.Meta)
}

func matchesStop(item stop, query string) bool {
	if strings.Contains(strings.ToLower(item.Name), query) {
		return true
	}
	for _, alias := range item.Aliases {
		if strings.Contains(strings.ToLower(alias), query) {
			return true
		}
	}
	return false
}

func decodeRequest(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return errors.New("payload is malformed")
	}
	return nil
}

func writeResult[T any](output io.Writer, result T) {
	payload, err := json.Marshal(result)
	if err != nil {
		writeEnvelope(output, resultEnvelope{Code: codeInternal, Message: "marshal result failed"})
		return
	}
	writeEnvelope(output, resultEnvelope{OK: true, Result: payload})
}

// fetchCollection 分页读取集合全部文档（文档与快照元数据由宿主单事务一并读出）。
func fetchCollection[T any](collection string) (collectionSnapshot[T], error) {
	var snapshot collectionSnapshot[T]
	var documents []T
	afterID := ""
	var sourceRevision string
	hasSourceRevision := false
	for {
		request, err := json.Marshal(storeListRequest{Collection: collection, Limit: 200, AfterID: afterID})
		if err != nil {
			return snapshot, err
		}
		response := callStore(request, storeList)
		if response == nil {
			return snapshot, errors.New("store list call failed")
		}
		var decoded storeListResponse
		if err := json.Unmarshal(response, &decoded); err != nil {
			return snapshot, err
		}
		if hasSourceRevision && decoded.Meta.SourceRevision != sourceRevision {
			return snapshot, errors.New(codeDataUnavailable)
		}
		if !hasSourceRevision {
			sourceRevision = decoded.Meta.SourceRevision
			hasSourceRevision = true
		}
		for _, document := range decoded.Docs {
			var item T
			if err := json.Unmarshal(document.Payload, &item); err != nil {
				return snapshot, err
			}
			documents = append(documents, item)
		}
		snapshot.Meta = decoded.Meta
		snapshot.MetaFound = decoded.MetaFound
		if len(decoded.Docs) < 200 {
			break
		}
		afterID = decoded.Docs[len(decoded.Docs)-1].ID
	}
	snapshot.Documents = documents
	return snapshot, nil
}

// callStore 以线性内存 ABI 调用宿主函数：请求与响应都位于 guest 线性内存。
func callStore(request []byte, importFn func(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32) []byte {
	if len(request) == 0 {
		request = []byte("{}")
	}
	response := make([]byte, maxStoreResponse)
	length := importFn(unsafe.Pointer(&request[0]), uint32(len(request)), unsafe.Pointer(&response[0]), uint32(len(response)))
	if length == hostFunctionError {
		return nil
	}
	return response[:length]
}

// writeEnvelope 序列化并写出结果信封。
func writeEnvelope(output io.Writer, envelope resultEnvelope) {
	data, err := json.Marshal(envelope)
	if err != nil {
		os.Stdout.Write([]byte(`{"ok":false,"code":"internal","message":"envelope marshal failed"}`))
		return
	}
	output.Write(data)
}
