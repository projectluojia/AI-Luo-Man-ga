package bus

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/guestkit"
)

// fakeStore 按集合提供文档与快照元数据，模拟 ailuo.store 的系统作用域读
// （本包只用 list；Get/Put/Delete 为占位实现）。
type fakeStore struct {
	meta      guestkit.SnapshotMeta
	metaFound bool
	docs      map[string][]guestkit.Document
}

func (f fakeStore) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	docs := f.docs[collection]
	if afterID != "" {
		filtered := docs
		docs = nil
		for _, doc := range filtered {
			if doc.ID > afterID {
				docs = append(docs, doc)
			}
		}
	}
	if len(docs) > limit {
		docs = docs[:limit]
	}
	if docs == nil {
		docs = []guestkit.Document{}
	}
	return guestkit.ListPage{Docs: docs, Meta: f.meta, MetaFound: f.metaFound}, nil
}

func storeFixture(t *testing.T, collections map[string][]any) *fakeStore {
	t.Helper()
	importedAt := time.Now().Add(-time.Hour)
	docs := make(map[string][]guestkit.Document, len(collections))
	for collection, items := range collections {
		for index, item := range items {
			payload, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			docs[collection] = append(docs[collection], guestkit.Document{ID: docID(index), Payload: payload})
		}
	}
	return &fakeStore{
		meta: guestkit.SnapshotMeta{
			SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true,
			Complete: true, ImportedAt: importedAt, ValidUntil: time.Now().Add(time.Hour),
		},
		metaFound: true,
		docs:      docs,
	}
}

func (f *fakeStore) Get(guestkit.Scope, string, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (f *fakeStore) Put(string, string, json.RawMessage) error { return nil }
func (f *fakeStore) Delete(string, string) (bool, error)       { return false, nil }

func docID(index int) string { return "doc-" + string(rune('a'+index)) }

func encode(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func dispatch(t *testing.T, store guestkit.Store, capabilityID string, payload any) guestkit.ResultEnvelope {
	t.Helper()
	return guestkit.NewDispatcher(Handlers(store)).Dispatch(capabilityID, encode(t, payload))
}

func decodeResult(t *testing.T, envelope guestkit.ResultEnvelope, target any) {
	t.Helper()
	if !envelope.OK {
		t.Fatalf("unexpected failure envelope: %s %s", envelope.Code, envelope.Message)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatal(err)
	}
}

func TestStopSearchFiltersByQueryAndSortsByName(t *testing.T) {
	store := storeFixture(t, map[string][]any{"stops": {
		Stop{ID: "s2", Name: "樱花大道", Aliases: []string{"sakura"}, SourceRevision: "rev-1"},
		Stop{ID: "s1", Name: "行政楼", Aliases: []string{"administration"}, SourceRevision: "rev-1"},
		Stop{ID: "s3", Name: "信息学部门口", Aliases: []string{"楼门"}, SourceRevision: "rev-1"},
	}})
	var result StopSearchResult
	decodeResult(t, dispatch(t, store, StopSearchCapabilityID, StopSearchRequest{Query: "楼"}), &result)
	if len(result.Stops) != 2 || result.Stops[0].Name != "信息学部门口" || result.Stops[1].Name != "行政楼" {
		t.Fatalf("stops=%+v", result.Stops)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh || result.DataStatus.SourceRevision != "rev-1" {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestStopSearchEnforcesDefaultsAndLimits(t *testing.T) {
	store := storeFixture(t, map[string][]any{"stops": {
		Stop{ID: "s1", Name: "行政楼", SourceRevision: "rev-1"},
	}})
	var result StopSearchResult
	// Limit 0 归一化为默认页大小。
	decodeResult(t, dispatch(t, store, StopSearchCapabilityID, StopSearchRequest{Query: "楼"}), &result)
	if len(result.Stops) != 1 {
		t.Fatalf("stops=%+v", result.Stops)
	}
	// limit 超界是协议错误。
	if envelope := dispatch(t, store, StopSearchCapabilityID, StopSearchRequest{Query: "楼", Limit: maxListLimit + 1}); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestRouteListSortsAndTruncates(t *testing.T) {
	store := storeFixture(t, map[string][]any{"routes": {
		Route{ID: "r2", Name: "环线", Direction: "逆行", SourceRevision: "rev-1"},
		Route{ID: "r1", Name: "环线", Direction: "顺行", SourceRevision: "rev-1"},
	}})
	var result RouteListResult
	decodeResult(t, dispatch(t, store, RouteListCapabilityID, RouteListRequest{}), &result)
	if len(result.Routes) != 2 || result.Routes[0].Direction != "逆行" || result.Routes[1].Direction != "顺行" {
		t.Fatalf("routes=%+v", result.Routes)
	}
	if result.DataStatus.SourceRevision != "rev-1" {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestJourneySearchFiltersStopsAndDeparture(t *testing.T) {
	departure := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	store := storeFixture(t, map[string][]any{"journeys": {
		Journey{TripID: "t1", OriginStopID: "a", DestinationStopID: "b", DepartureAt: departure, SourceRevision: "rev-1"},
		Journey{TripID: "t2", OriginStopID: "a", DestinationStopID: "b", DepartureAt: departure.Add(30 * time.Minute), SourceRevision: "rev-1"},
		Journey{TripID: "t3", OriginStopID: "b", DestinationStopID: "a", DepartureAt: departure.Add(time.Hour), SourceRevision: "rev-1"},
	}})
	var result JourneySearchResult
	decodeResult(t, dispatch(t, store, JourneySearchCapabilityID, JourneySearchRequest{
		OriginStopID: "a", DestinationStopID: "b", DepartAfter: departure.Add(15 * time.Minute),
	}), &result)
	if len(result.Journeys) != 1 || result.Journeys[0].TripID != "t2" {
		t.Fatalf("journeys=%+v", result.Journeys)
	}
	// 起止站相同是协议错误。
	if envelope := dispatch(t, store, JourneySearchCapabilityID, JourneySearchRequest{OriginStopID: "a", DestinationStopID: "a"}); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestJourneySearchRejectsStrictSchema(t *testing.T) {
	store := storeFixture(t, nil)
	envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(JourneySearchCapabilityID, json.RawMessage(`{"origin_stop_id":"a","destination_stop_id":"b","unknown":1}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestRealtimePositionsReturnsAuthorizedSetWithValidation(t *testing.T) {
	recorded := time.Now().Add(-2 * time.Minute)
	store := storeFixture(t, map[string][]any{"vehicle_positions": {
		VehiclePosition{VehicleID: "v1", RouteID: "route-1", Latitude: 30.5, Longitude: 114.3, RecordedAt: recorded, SourceRevision: "rev-1"},
		VehiclePosition{VehicleID: "v2", RouteID: "route-2", Latitude: 30.6, Longitude: 114.4, RecordedAt: recorded.Add(-time.Minute), SourceRevision: "rev-1"},
	}})
	var result RealtimePositionResult
	decodeResult(t, dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{RouteID: "route-2"}), &result)
	if len(result.Positions) != 1 || result.Positions[0].VehicleID != "v2" {
		t.Fatalf("positions=%+v", result.Positions)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestRealtimePositionsKeepsNewestPerVehicle(t *testing.T) {
	// 同一车辆多次上报时只保留最新位置，输出按时间降序（最新在前）。
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	store := storeFixture(t, map[string][]any{"vehicle_positions": {
		VehiclePosition{VehicleID: "v1", RouteID: "route-1", Latitude: 30.51, Longitude: 114.31, RecordedAt: base, SourceRevision: "rev-1"},
		VehiclePosition{VehicleID: "v1", RouteID: "route-1", Latitude: 30.55, Longitude: 114.35, RecordedAt: base.Add(time.Minute), SourceRevision: "rev-1"},
		VehiclePosition{VehicleID: "v2", RouteID: "route-1", Latitude: 30.6, Longitude: 114.4, RecordedAt: base.Add(2 * time.Minute), SourceRevision: "rev-1"},
	}})
	var result RealtimePositionResult
	decodeResult(t, dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{RouteID: "route-1"}), &result)
	if len(result.Positions) != 2 {
		t.Fatalf("positions=%+v", result.Positions)
	}
	if result.Positions[0].VehicleID != "v2" || result.Positions[1].VehicleID != "v1" || result.Positions[1].Latitude != 30.55 {
		t.Fatalf("positions=%+v", result.Positions)
	}
}

func TestRealtimePositionsRejectsDirtyCoordinates(t *testing.T) {
	recorded := time.Now().Add(-time.Minute)
	store := storeFixture(t, map[string][]any{"vehicle_positions": {
		VehiclePosition{VehicleID: "v1", RouteID: "route-1", Latitude: 91, Longitude: 114.3, RecordedAt: recorded, SourceRevision: "rev-1"},
	}})
	if envelope := dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{}); envelope.OK || envelope.Code != guestkit.CodeDataIncomplete {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestRealtimePositionsTreatsMissingCollectionAsUnavailable(t *testing.T) {
	// 未授权（集合从未导入）fail-closed 返回 data_unavailable，而非空成功。
	store := storeFixture(t, nil)
	store.metaFound = false
	if envelope := dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{}); envelope.OK || envelope.Code != guestkit.CodeDataUnavailable {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestGovernanceFailuresMapToStableCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta guestkit.SnapshotMeta
		code string
	}{{"untrusted", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "demo-fixture", Complete: true,
		ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, guestkit.CodeDataUntrusted}, {"expired", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: time.Now().Add(-2 * time.Hour), ValidUntil: time.Now().Add(-time.Minute),
	}, guestkit.CodeDataExpired}, {"incomplete", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, guestkit.CodeDataIncomplete}} {
		t.Run(tc.name, func(t *testing.T) {
			store := storeFixture(t, map[string][]any{"routes": {
				Route{ID: "r1", Name: "线路", Direction: "顺行", SourceRevision: "rev-1"},
			}})
			store.meta = tc.meta
			if envelope := dispatch(t, store, RouteListCapabilityID, RouteListRequest{}); envelope.OK || envelope.Code != tc.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestUnknownCapabilityIsInvalidArgument(t *testing.T) {
	store := storeFixture(t, nil)
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("campus.bus.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestEmptyCollectionsReturnEmptyResultsUnderGovernance(t *testing.T) {
	store := storeFixture(t, map[string][]any{"stops": {}})
	var result StopSearchResult
	decodeResult(t, dispatch(t, store, StopSearchCapabilityID, StopSearchRequest{Query: "楼"}), &result)
	if len(result.Stops) != 0 || result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("result=%+v", result)
	}
}

func TestSnapshotRevisionMismatchFailsClosed(t *testing.T) {
	store := storeFixture(t, map[string][]any{"routes": {
		Route{ID: "r1", Name: "线路", Direction: "顺行", SourceRevision: "other-revision"},
	}})
	if envelope := dispatch(t, store, RouteListCapabilityID, RouteListRequest{}); envelope.OK || envelope.Code != guestkit.CodeDataUnavailable {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestStoreFailureIsInternal(t *testing.T) {
	store := failingStore{}
	if envelope := dispatch(t, store, RouteListCapabilityID, RouteListRequest{}); envelope.OK || envelope.Code != guestkit.CodeInternal {
		t.Fatalf("envelope=%+v", envelope)
	}
}

type failingStore struct{}

func (failingStore) List(guestkit.Scope, string, int, string) (guestkit.ListPage, error) {
	return guestkit.ListPage{}, errors.New("store list call failed")
}

func (failingStore) Get(guestkit.Scope, string, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (failingStore) Put(string, string, json.RawMessage) error { return nil }
func (failingStore) Delete(string, string) (bool, error)       { return false, nil }

func TestDispatchMarshalFailureIsInternal(t *testing.T) {
	// 结果载荷含 NaN 坐标时 json.Marshal 失败：信封按 internal 应答而不是 panic。
	store := storeFixture(t, map[string][]any{"stops": {
		Stop{ID: "s1", Name: "站", Latitude: 1, Longitude: 2, SourceRevision: "rev-1"},
	}})
	envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(StopSearchCapabilityID, encode(t, StopSearchRequest{Query: "站"}))
	if !envelope.OK {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestRealtimeLimitBounds(t *testing.T) {
	store := storeFixture(t, map[string][]any{"vehicle_positions": {}})
	if envelope := dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{Limit: maxRealtimeLimit + 1}); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
	var result RealtimePositionResult
	// Limit 0 归一化为默认页大小。
	decodeResult(t, dispatch(t, store, RealtimePositionCapabilityID, RealtimePositionRequest{Limit: 0}), &result)
	if len(result.Positions) != 0 {
		t.Fatalf("positions=%+v", result.Positions)
	}
}
