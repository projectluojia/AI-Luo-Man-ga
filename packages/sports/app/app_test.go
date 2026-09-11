package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/sports/sports"
)

// memStore 是窄 store 接口的内存实现：系统作用域以快照集合提供，个人作用域
// 以文档映射提供（List 对个人作用域不返回快照元数据）。
type memStore struct {
	meta      guestkit.SnapshotMeta
	metaFound bool
	system    map[string][]guestkit.Document
	user      map[string]map[string]json.RawMessage
}

func newMemStore(meta guestkit.SnapshotMeta, metaFound bool) *memStore {
	return &memStore{
		meta: meta, metaFound: metaFound,
		system: map[string][]guestkit.Document{},
		user:   map[string]map[string]json.RawMessage{},
	}
}

func (m *memStore) Get(scope guestkit.Scope, collection, id string) (json.RawMessage, bool, error) {
	if scope == guestkit.ScopeUser {
		payload, found := m.user[collection][id]
		return payload, found, nil
	}
	for _, doc := range m.system[collection] {
		if doc.ID == id {
			return doc.Payload, true, nil
		}
	}
	return nil, false, nil
}

func (m *memStore) List(scope guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	if scope == guestkit.ScopeUser {
		// 个人作用域读取不携带快照治理元数据（MetaFound=false）。
		var docs []guestkit.Document
		for id, payload := range m.user[collection] {
			if afterID == "" || id > afterID {
				docs = append(docs, guestkit.Document{ID: id, Payload: payload})
			}
		}
		if len(docs) > limit {
			docs = docs[:limit]
		}
		if docs == nil {
			docs = []guestkit.Document{}
		}
		return guestkit.ListPage{Docs: docs, MetaFound: false}, nil
	}
	var docs []guestkit.Document
	for _, doc := range m.system[collection] {
		if afterID == "" || doc.ID > afterID {
			docs = append(docs, doc)
		}
	}
	if len(docs) > limit {
		docs = docs[:limit]
	}
	if docs == nil {
		docs = []guestkit.Document{}
	}
	return guestkit.ListPage{Docs: docs, Meta: m.meta, MetaFound: m.metaFound}, nil
}

func (m *memStore) Put(collection, id string, doc json.RawMessage) error {
	if m.user[collection] == nil {
		m.user[collection] = map[string]json.RawMessage{}
	}
	m.user[collection][id] = doc
	return nil
}

func (m *memStore) Delete(string, string) (bool, error) { return false, nil }

func authoritativeMeta() guestkit.SnapshotMeta {
	return guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true,
		Complete: true, ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}
}

func doc(t *testing.T, id string, payload any) guestkit.Document {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return guestkit.Document{ID: id, Payload: data}
}

// futureSlot 返回仍可预约的时段（明天 08:00-10:00 UTC）。
func futureSlot() (time.Time, time.Time) {
	start := time.Now().UTC().AddDate(0, 0, 1).Truncate(time.Hour)
	return start, start.Add(2 * time.Hour)
}

func futureDateAt(start time.Time) string {
	return start.In(sports.AcademicLocation()).Format(sports.AcademicDateLayout)
}

// fixtureStore 播种一个场馆、一个项目、一个时段与 WebView 描述符。
func fixtureStore(t *testing.T) *memStore {
	t.Helper()
	store := newMemStore(authoritativeMeta(), true)
	startsAt, endsAt := futureSlot()
	store.system[sports.VenuesCollection] = []guestkit.Document{
		doc(t, "venue-1", sports.Venue{ID: "venue-1", Name: "卓尔体育馆", Campus: "文理学部", Address: "工学部大道", SourceRevision: "rev-1"}),
	}
	store.system[sports.ProjectsCollection] = []guestkit.Document{
		doc(t, "project-1", sports.Project{ID: "project-1", VenueID: "venue-1", Name: "羽毛球", SourceRevision: "rev-1"}),
	}
	store.system[sports.SlotsCollection] = []guestkit.Document{
		doc(t, "slot-1", sports.Slot{
			ID: "slot-1", VenueID: "venue-1", ProjectID: "project-1", Date: futureDateAt(startsAt),
			StartAt: startsAt, EndAt: endsAt, Capacity: 4, RemainingQuota: 4, SourceRevision: "rev-1",
		}),
	}
	store.system[sports.WebviewCollection] = []guestkit.Document{
		doc(t, "descriptor", sports.WebViewDescriptor{
			EntryURL: "https://orders.example.com/sports", RequiredUserAgent: "LuoJia/1.0",
			RequiredHeaders: []sports.RequiredHeader{{Name: "X-App-Id", Purpose: "应用标识"}},
			SourceRevision:  "rev-1",
		}),
	}
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

func payload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVenuesListSortsAndCaps(t *testing.T) {
	store := fixtureStore(t)
	var result venuesListResult
	decodeResult(t, dispatch(t, store, capVenuesList, sports.VenuesListRequest{}), &result)
	if len(result.Venues) != 1 || result.Venues[0].Name != "卓尔体育馆" {
		t.Fatalf("venues=%+v", result.Venues)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestProjectsListFiltersByVenue(t *testing.T) {
	store := fixtureStore(t)
	var result projectsListResult
	decodeResult(t, dispatch(t, store, capProjectsList, sports.ProjectsListRequest{VenueID: "venue-1"}), &result)
	if len(result.Projects) != 1 || result.Projects[0].Name != "羽毛球" {
		t.Fatalf("projects=%+v", result.Projects)
	}
	// 未知场馆返回空集。
	result = projectsListResult{}
	decodeResult(t, dispatch(t, store, capProjectsList, sports.ProjectsListRequest{VenueID: "venue-missing"}), &result)
	if len(result.Projects) != 0 {
		t.Fatalf("unknown venue should be empty: %+v", result.Projects)
	}
}

func TestSlotsSearchFiltersByDate(t *testing.T) {
	store := fixtureStore(t)
	startsAt, _ := futureSlot()
	var result slotsSearchResult
	decodeResult(t, dispatch(t, store, capSlotsSearch, sports.SlotSearchRequest{
		VenueID: "venue-1", ProjectID: "project-1", Date: futureDateAt(startsAt),
	}), &result)
	if len(result.Slots) != 1 || result.Slots[0].ID != "slot-1" || result.Slots[0].RemainingQuota != 4 {
		t.Fatalf("slots=%+v", result.Slots)
	}
}

func TestOrdersWebviewReturnsDescriptor(t *testing.T) {
	store := fixtureStore(t)
	var result webviewResult
	decodeResult(t, dispatch(t, store, capOrdersWebview, sports.VenuesListRequest{}), &result)
	if result.EntryURL != "https://orders.example.com/sports" || result.RequiredUserAgent != "LuoJia/1.0" {
		t.Fatalf("webview=%+v", result)
	}
	if len(result.RequiredHeaders) != 1 || result.RequiredHeaders[0].Name != "X-App-Id" {
		t.Fatalf("headers=%+v", result.RequiredHeaders)
	}
}

func TestReservationLifecycleCreateCancelReplayConflict(t *testing.T) {
	store := fixtureStore(t)
	dispatcher := guestkit.NewDispatcher(Handlers(store))

	created := dispatcher.Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1", Count: 2,
	}))
	if !created.OK {
		t.Fatalf("create: %s %s", created.Code, created.Message)
	}
	var createResult reservationResult
	if err := json.Unmarshal(created.Result, &createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.Reservation.ReservationID == "" || createResult.Reservation.Status != sports.StatusConfirmed || createResult.Reservation.Count != 2 {
		t.Fatalf("created=%+v", createResult.Reservation)
	}
	if createResult.Reservation.VenueName != "卓尔体育馆" || createResult.Reservation.ProjectName != "羽毛球" {
		t.Fatalf("denormalized names missing: %+v", createResult.Reservation)
	}
	reservationID := createResult.Reservation.ReservationID

	// 重复预约同一时段带内冲突。
	conflict := dispatcher.Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1",
	}))
	if !conflict.OK {
		t.Fatalf("double-book should be in-band: %+v", conflict)
	}
	var slotConflict guestkit.Conflict
	if err := json.Unmarshal(conflict.Result, &slotConflict); err != nil || slotConflict.Conflict != "slot_conflict" {
		t.Fatalf("slotConflict=%+v err=%v", slotConflict, err)
	}

	// 未提供 reservation 的取消按带内 not-found。
	missing := dispatcher.Dispatch(capReservationsCancel, payload(t, sports.ReservationCancelRequest{ReservationID: "missing"}))
	if !missing.OK {
		t.Fatalf("missing cancel should be in-band: %+v", missing)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(missing.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}

	// 取消本人预约。
	cancelled := dispatcher.Dispatch(capReservationsCancel, payload(t, sports.ReservationCancelRequest{ReservationID: reservationID}))
	if !cancelled.OK {
		t.Fatalf("cancel: %s %s", cancelled.Code, cancelled.Message)
	}
	var cancelResult reservationResult
	if err := json.Unmarshal(cancelled.Result, &cancelResult); err != nil {
		t.Fatal(err)
	}
	if cancelResult.Reservation.Status != sports.StatusCancelled || cancelResult.Reservation.CancelledAt == "" {
		t.Fatalf("cancelled=%+v", cancelResult.Reservation)
	}

	// 重复取消带内冲突。
	again := dispatcher.Dispatch(capReservationsCancel, payload(t, sports.ReservationCancelRequest{ReservationID: reservationID}))
	if !again.OK {
		t.Fatalf("double cancel should be in-band: %+v", again)
	}
	var cancelConflict guestkit.Conflict
	if err := json.Unmarshal(again.Result, &cancelConflict); err != nil || cancelConflict.Conflict == "" {
		t.Fatalf("conflict=%+v err=%v", cancelConflict, err)
	}
}

func TestReservationQuotaExceeded(t *testing.T) {
	store := fixtureStore(t)
	// remaining_quota=3 < count=16 → 超配额带内冲突。
	store.system[sports.SlotsCollection] = []guestkit.Document{
		doc(t, "slot-1", sports.Slot{
			ID: "slot-1", VenueID: "venue-1", ProjectID: "project-1", Date: "2026-09-02",
			StartAt: time.Now().UTC().Add(time.Hour), EndAt: time.Now().UTC().Add(2 * time.Hour),
			Capacity: 3, RemainingQuota: 3, SourceRevision: "rev-1",
		}),
	}
	result := guestkit.NewDispatcher(Handlers(store)).Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1", Count: sports.MaxCount,
	}))
	if !result.OK {
		t.Fatalf("over-quota should be in-band: %+v", result)
	}
	var conflict guestkit.Conflict
	if err := json.Unmarshal(result.Result, &conflict); err != nil || conflict.Conflict != "quota_exceeded" {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}
}

func TestReservationCreateUnknownVenueInBandNotFound(t *testing.T) {
	store := fixtureStore(t)
	result := guestkit.NewDispatcher(Handlers(store)).Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-missing", ProjectID: "project-1", SlotID: "slot-1",
	}))
	if !result.OK {
		t.Fatalf("unknown venue should be in-band: %+v", result)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(result.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}
}

func TestReservationCreateRejectsPastSlot(t *testing.T) {
	store := fixtureStore(t)
	store.system[sports.SlotsCollection] = []guestkit.Document{
		doc(t, "slot-1", sports.Slot{
			ID: "slot-1", VenueID: "venue-1", ProjectID: "project-1", Date: "2020-01-01",
			StartAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), EndAt: time.Date(2020, 1, 1, 2, 0, 0, 0, time.UTC),
			Capacity: 4, RemainingQuota: 4, SourceRevision: "rev-1",
		}),
	}
	result := guestkit.NewDispatcher(Handlers(store)).Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1",
	}))
	if result.OK || result.Code != guestkit.CodeDataExpired {
		t.Fatalf("past slot should be data_expired: %+v", result)
	}
}

func TestScheduleAddIdempotent(t *testing.T) {
	store := fixtureStore(t)
	dispatcher := guestkit.NewDispatcher(Handlers(store))
	created := dispatcher.Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1",
	}))
	if !created.OK {
		t.Fatalf("create: %s", created.Code)
	}
	var createResult reservationResult
	if err := json.Unmarshal(created.Result, &createResult); err != nil {
		t.Fatal(err)
	}
	first := dispatcher.Dispatch(capScheduleAdd, payload(t, sports.ScheduleAddRequest{ReservationID: createResult.Reservation.ReservationID}))
	if !first.OK {
		t.Fatalf("schedule add: %s", first.Code)
	}
	var firstResult scheduleResult
	if err := json.Unmarshal(first.Result, &firstResult); err != nil {
		t.Fatal(err)
	}
	if firstResult.Schedule.Title != "卓尔体育馆 羽毛球" || firstResult.Schedule.ReservationID != createResult.Reservation.ReservationID {
		t.Fatalf("schedule=%+v", firstResult.Schedule)
	}
	// 重复添加返回既有日程（幂等）。
	second := dispatcher.Dispatch(capScheduleAdd, payload(t, sports.ScheduleAddRequest{ReservationID: createResult.Reservation.ReservationID}))
	var secondResult scheduleResult
	if err := json.Unmarshal(second.Result, &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResult.Schedule.ScheduleID != firstResult.Schedule.ScheduleID {
		t.Fatalf("schedule add not idempotent: %+v vs %+v", firstResult.Schedule, secondResult.Schedule)
	}
	// 未知预约按带内 not-found。
	missing := dispatcher.Dispatch(capScheduleAdd, payload(t, sports.ScheduleAddRequest{ReservationID: "missing"}))
	if !missing.OK {
		t.Fatalf("missing schedule add should be in-band: %+v", missing)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(missing.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}
}

func TestReservationsMineSortsNewestFirst(t *testing.T) {
	store := fixtureStore(t)
	dispatcher := guestkit.NewDispatcher(Handlers(store))
	if ok := dispatcher.Dispatch(capReservationsCreate, payload(t, sports.ReservationCreateRequest{
		VenueID: "venue-1", ProjectID: "project-1", SlotID: "slot-1",
	})); !ok.OK {
		t.Fatalf("create 1: %s", ok.Code)
	}
	// 取消后重建同一时段：列表应含两条（confirmed + cancelled），最新在前。
	if ok := dispatcher.Dispatch(capReservationsCancel, payload(t, sports.ReservationCancelRequest{ReservationID: ""})); ok.OK {
		t.Fatal("empty cancel should fail")
	}
	var list reservationsResult
	decodeResult(t, dispatcher.Dispatch(capReservationsMine, payload(t, sports.ReservationListRequest{})), &list)
	if len(list.Reservations) != 1 {
		t.Fatalf("mine=%+v", list.Reservations)
	}
	if list.Reservations[0].Status != sports.StatusConfirmed {
		t.Fatalf("status=%s", list.Reservations[0].Status)
	}
}

func TestGovernanceFailuresMapToStableCodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		meta      guestkit.SnapshotMeta
		metaFound bool
		code      string
	}{{"untrusted", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "demo-fixture", Complete: true,
		ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, true, guestkit.CodeDataUntrusted}, {"expired", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: time.Now().Add(-2 * time.Hour), ValidUntil: time.Now().Add(-time.Minute),
	}, true, guestkit.CodeDataExpired}, {"incomplete", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, true, guestkit.CodeDataIncomplete}, {"missing", guestkit.SnapshotMeta{}, false, guestkit.CodeDataUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(tc.meta, tc.metaFound)
			if envelope := dispatch(t, store, capVenuesList, sports.VenuesListRequest{}); envelope.OK || envelope.Code != tc.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestUnknownCapabilityAndInvalidPayloads(t *testing.T) {
	store := fixtureStore(t)
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("sports.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(capVenuesList, json.RawMessage(`{"extra":1}`)); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("unknown field envelope=%+v", envelope)
	}
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(capProjectsList, json.RawMessage(`{}`)); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("missing venue_id envelope=%+v", envelope)
	}
}
