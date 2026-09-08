package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/projectluojia/classroom/classroom"
	"github.com/projectluojia/guestkit"
)

// memStore 是窄 store 接口的内存实现：系统作用域以快照集合提供，个人作用域
// 以文档 map 提供，模拟宿主 ailuo.store 的隔离与治理语义。
type memStore struct {
	meta      guestkit.SnapshotMeta
	metaFound bool
	system    map[string][]guestkit.Document
	user      map[string]map[string]json.RawMessage
}

func newMemStore(meta guestkit.SnapshotMeta) *memStore {
	return &memStore{
		meta: meta, metaFound: true,
		system: map[string][]guestkit.Document{},
		user:   map[string]map[string]json.RawMessage{},
	}
}

func (m *memStore) seed(collections map[string][]any) {
	for collection, items := range collections {
		for index, item := range items {
			payload, err := json.Marshal(item)
			if err != nil {
				panic(err)
			}
			m.system[collection] = append(m.system[collection], guestkit.Document{ID: docID(index), Payload: payload})
		}
	}
}

func docID(index int) string { return "doc-" + string(rune('a'+index)) }

func (m *memStore) Get(_ guestkit.Scope, collection, id string) (json.RawMessage, bool, error) {
	payload, ok := m.user[collection][id]
	return payload, ok, nil
}

func (m *memStore) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	var docs []guestkit.Document
	if _, isUser := m.user[collection]; isUser {
		for id, payload := range m.user[collection] {
			if afterID == "" || id > afterID {
				docs = append(docs, guestkit.Document{ID: id, Payload: payload})
			}
		}
	} else {
		for _, doc := range m.system[collection] {
			if afterID == "" || doc.ID > afterID {
				docs = append(docs, doc)
			}
		}
	}
	// 稳定排序（map 迭代无序，测试断言需要确定序）。
	for i := 1; i < len(docs); i++ {
		for j := i; j > 0 && docs[j].ID < docs[j-1].ID; j-- {
			docs[j], docs[j-1] = docs[j-1], docs[j]
		}
	}
	if len(docs) > limit {
		docs = docs[:limit]
	}
	if docs == nil {
		docs = []guestkit.Document{}
	}
	metaFound := m.metaFound
	meta := m.meta
	if _, isUser := m.user[collection]; isUser {
		metaFound = false
		meta = guestkit.SnapshotMeta{}
	}
	return guestkit.ListPage{Docs: docs, Meta: meta, MetaFound: metaFound}, nil
}

func (m *memStore) Put(collection, id string, doc json.RawMessage) error {
	if m.user[collection] == nil {
		m.user[collection] = map[string]json.RawMessage{}
	}
	m.user[collection][id] = doc
	return nil
}

func (m *memStore) Delete(collection, id string) (bool, error) {
	if _, ok := m.user[collection][id]; !ok {
		return false, nil
	}
	delete(m.user[collection], id)
	return true, nil
}

func authoritativeMeta() guestkit.SnapshotMeta {
	return guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true,
		Complete: true, ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}
}

func fixtureStore(t *testing.T) *memStore {
	t.Helper()
	store := newMemStore(authoritativeMeta())
	store.seed(map[string][]any{
		"campuses": {classroom.Campus{ID: "campus-a", Name: "文理学部", SourceRevision: "rev-1"}},
		"buildings": {
			classroom.Building{ID: "b1", CampusID: "campus-a", Name: "教五", SourceRevision: "rev-1"},
			classroom.Building{ID: "b2", CampusID: "campus-a", Name: "一教", SourceRevision: "rev-1"},
		},
		"rooms": {
			classroom.Room{ID: "room-1", CampusID: "campus-a", BuildingID: "b1", Name: "教五-101", SourceRevision: "rev-1"},
			classroom.Room{ID: "room-2", CampusID: "campus-a", BuildingID: "b2", Name: "一教-101", SourceRevision: "rev-1"},
		},
		"occupancy": {
			classroom.Occupancy{RoomID: "room-1", AcademicDate: "2026-09-07", Period: 1, SourceRevision: "rev-1"},
		},
	})
	return store
}

func dispatch(t *testing.T, store guestkit.Store, capabilityID string, payload any) guestkit.ResultEnvelope {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return guestkit.NewDispatcher(Handlers(store)).Dispatch(capabilityID, body)
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

func TestRoomsSearchExcludesOccupiedAndFiltersBuilding(t *testing.T) {
	store := fixtureStore(t)
	var result roomsSearchResult
	decodeResult(t, dispatch(t, store, "classroom.rooms.search", classroom.RoomsSearchRequest{Date: "2026-09-07", CampusID: "campus-a", Period: 1}), &result)
	if len(result.Rooms) != 1 || result.Rooms[0].ID != "room-2" {
		t.Fatalf("rooms=%+v", result.Rooms)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
	// 无占用节次两间都空闲。
	decodeResult(t, dispatch(t, store, "classroom.rooms.search", classroom.RoomsSearchRequest{Date: "2026-09-07", CampusID: "campus-a", Period: 2}), &result)
	if len(result.Rooms) != 2 {
		t.Fatalf("rooms=%+v", result.Rooms)
	}
	// 教学楼过滤。
	decodeResult(t, dispatch(t, store, "classroom.rooms.search", classroom.RoomsSearchRequest{Date: "2026-09-07", CampusID: "campus-a", BuildingID: "b1", Period: 2}), &result)
	if len(result.Rooms) != 1 || result.Rooms[0].ID != "room-1" {
		t.Fatalf("rooms=%+v", result.Rooms)
	}
}

func TestRoomsSearchRejectsInvalidPayloads(t *testing.T) {
	store := fixtureStore(t)
	for name, request := range map[string]classroom.RoomsSearchRequest{
		"bad date":    {Date: "2026-9-7", CampusID: "campus-a", Period: 1},
		"period zero": {Date: "2026-09-07", CampusID: "campus-a"},
		"no campus":   {Date: "2026-09-07", Period: 1},
		"limit over":  {Date: "2026-09-07", CampusID: "campus-a", Period: 1, Limit: classroom.MaxListLimit + 1},
	} {
		if envelope := dispatch(t, store, "classroom.rooms.search", request); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
			t.Fatalf("%s: envelope=%+v", name, envelope)
		}
	}
	// 未知字段是协议违例。
	envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("classroom.rooms.search", json.RawMessage(`{"date":"2026-09-07","campus_id":"c","period":1,"extra":true}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestCampusAndBuildingList(t *testing.T) {
	store := fixtureStore(t)
	var campuses campusesListResult
	decodeResult(t, dispatch(t, store, "classroom.campuses.list", classroom.CampusListRequest{}), &campuses)
	if len(campuses.Campuses) != 1 || campuses.Campuses[0].Name != "文理学部" {
		t.Fatalf("campuses=%+v", campuses.Campuses)
	}
	var buildings buildingsListResult
	decodeResult(t, dispatch(t, store, "classroom.buildings.list", classroom.BuildingListRequest{CampusID: "campus-a"}), &buildings)
	if len(buildings.Buildings) != 2 {
		t.Fatalf("buildings=%+v", buildings.Buildings)
	}
	// 其他校区无教学楼。
	decodeResult(t, dispatch(t, store, "classroom.buildings.list", classroom.BuildingListRequest{CampusID: "campus-other"}), &buildings)
	if len(buildings.Buildings) != 0 {
		t.Fatalf("buildings=%+v", buildings.Buildings)
	}
}

func TestScheduleLifecycleCreateListCancel(t *testing.T) {
	store := fixtureStore(t)
	var created scheduleCreateResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{RoomID: "room-2", Date: "2026-09-08", Period: 3}), &created)
	if created.Schedule.ScheduleID == "" || created.Schedule.Status != classroom.StatusScheduled {
		t.Fatalf("created=%+v", created.Schedule)
	}
	if created.Schedule.Title != classroom.DefaultTitle("room-2", "2026-09-08", 3) {
		t.Fatalf("default title=%q", created.Schedule.Title)
	}
	// 不存在的教室 → 带内 found=false。
	var missing guestkit.NotFound
	decodeResult(t, dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{RoomID: "room-x", Date: "2026-09-08", Period: 3}), &missing)
	if missing.Found {
		t.Fatal("missing room reported found")
	}
	// 列出日程。
	var listed scheduleListResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.list", classroom.ScheduleListRequest{}), &listed)
	if len(listed.Schedules) != 1 || listed.Schedules[0].ScheduleID != created.Schedule.ScheduleID {
		t.Fatalf("listed=%+v", listed.Schedules)
	}
	// 取消。
	var cancelled scheduleCancelResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.cancel", classroom.ScheduleCancelRequest{ScheduleID: created.Schedule.ScheduleID}), &cancelled)
	if cancelled.Schedule.Status != classroom.StatusCancelled || cancelled.Schedule.CancelledAt == "" {
		t.Fatalf("cancelled=%+v", cancelled.Schedule)
	}
	// 重复取消 → 带内 conflict。
	var conflict guestkit.Conflict
	decodeResult(t, dispatch(t, store, "classroom.schedule.cancel", classroom.ScheduleCancelRequest{ScheduleID: created.Schedule.ScheduleID}), &conflict)
	if conflict.Conflict == "" {
		t.Fatal("expected conflict on double cancel")
	}
	// 不存在的日程取消 → 带内 found=false。
	decodeResult(t, dispatch(t, store, "classroom.schedule.cancel", classroom.ScheduleCancelRequest{ScheduleID: "s-missing"}), &missing)
	if missing.Found {
		t.Fatal("missing schedule reported found")
	}
}

func TestScheduleWriteFailsClosedWhenStoreRejects(t *testing.T) {
	store := fixtureStore(t)
	envelope := dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{RoomID: "room-2", Date: "2026-09-08", Period: 3})
	if !envelope.OK {
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
	}, guestkit.CodeDataIncomplete}, {"missing", guestkit.SnapshotMeta{}, guestkit.CodeDataUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(tc.meta)
			store.metaFound = tc.name != "missing"
			store.seed(map[string][]any{"rooms": {classroom.Room{ID: "room-1", CampusID: "c", Name: "r", SourceRevision: "rev-1"}}})
			if envelope := dispatch(t, store, "classroom.rooms.search", classroom.RoomsSearchRequest{Date: "2026-09-07", CampusID: "c", Period: 1}); envelope.OK || envelope.Code != tc.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestSnapshotRevisionMismatchFailsClosed(t *testing.T) {
	store := fixtureStore(t)
	store.seed(map[string][]any{"rooms": {classroom.Room{ID: "room-9", CampusID: "campus-a", Name: "杂散", SourceRevision: "other"}}})
	if envelope := dispatch(t, store, "classroom.rooms.search", classroom.RoomsSearchRequest{Date: "2026-09-07", CampusID: "campus-a", Period: 1}); envelope.OK || envelope.Code != guestkit.CodeDataUnavailable {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestUnknownCapabilityIsInvalidArgument(t *testing.T) {
	store := fixtureStore(t)
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("classroom.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestScheduleListSortedByDateThenPeriod(t *testing.T) {
	store := fixtureStore(t)
	// 先建晚的再建早的，断言列表按日期+节次排序。
	var late scheduleCreateResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{RoomID: "room-2", Date: "2026-09-09", Period: 2}), &late)
	var early scheduleCreateResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{RoomID: "room-2", Date: "2026-09-08", Period: 8}), &early)
	var listed scheduleListResult
	decodeResult(t, dispatch(t, store, "classroom.schedule.list", classroom.ScheduleListRequest{}), &listed)
	if len(listed.Schedules) != 2 || listed.Schedules[0].ScheduleID != early.Schedule.ScheduleID {
		t.Fatalf("listed=%+v", listed.Schedules)
	}
}

func TestScheduleCreateEnforcesQuota(t *testing.T) {
	store := fixtureStore(t)
	for index := 0; index < classroom.MaxSchedulesPerUser; index++ {
		envelope := dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{
			RoomID: "room-2", Date: "2026-09-09", Period: (index % 8) + 1,
		})
		if !envelope.OK {
			t.Fatalf("seed create %d: %+v", index, envelope)
		}
	}
	// 超限是带内业务失败：ok:true + capacity_exceeded。
	envelope := dispatch(t, store, "classroom.schedule.create", classroom.ScheduleCreateRequest{
		RoomID: "room-2", Date: "2026-09-10", Period: 1,
	})
	if !envelope.OK {
		t.Fatalf("quota exceeded must stay in-band: envelope=%+v", envelope)
	}
	var result map[string]any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["capacity_exceeded"] != true {
		t.Fatalf("quota result=%+v", result)
	}
}

func TestModelValidationBoundaries(t *testing.T) {
	now := "2026-09-07"
	for name, request := range map[string]classroom.ScheduleCreateRequest{
		"missing room": {Date: now, Period: 1},
		"bad period":   {RoomID: "r", Date: now, Period: classroom.MaxPeriod + 1},
		"title over":   {RoomID: "r", Date: now, Period: 1, Title: makeString(classroom.MaxTitleLength + 1)},
		"room id over": {RoomID: makeString(129), Date: now, Period: 1},
	} {
		if err := request.NormalizeAndValidate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	cancel := classroom.ScheduleCancelRequest{ScheduleID: ""}
	if err := cancel.NormalizeAndValidate(); err == nil {
		t.Fatal("empty cancel id accepted")
	}
}

func makeString(length int) string {
	bytes := make([]byte, length)
	for i := range bytes {
		bytes[i] = 'a'
	}
	return string(bytes)
}
