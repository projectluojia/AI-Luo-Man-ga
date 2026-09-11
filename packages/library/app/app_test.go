package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/library/library"
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

func (m *memStore) Get(guestkit.Scope, string, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (m *memStore) List(scope guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	if scope == guestkit.ScopeUser {
		// 个人作用域读取不携带快照治理元数据（MetaFound=false）。
		return guestkit.ListPage{Docs: []guestkit.Document{}, MetaFound: false}, nil
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

// userReservationStore 在 memStore 上叠加个人预约读取：List 的个人作用域返回
// 已写入的预约文档（模拟宿主按 UserID 隔离后的读取）。
type userReservationStore struct {
	*memStore
}

func (u *userReservationStore) List(scope guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	if scope == guestkit.ScopeUser && collection == library.ReservationsCollection {
		var docs []guestkit.Document
		for id, payload := range u.user[collection] {
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
	return u.memStore.List(scope, collection, limit, afterID)
}

func (u *userReservationStore) Get(scope guestkit.Scope, collection, id string) (json.RawMessage, bool, error) {
	if scope == guestkit.ScopeUser && collection == library.ReservationsCollection {
		payload, found := u.user[collection][id]
		return payload, found, nil
	}
	return u.memStore.Get(scope, collection, id)
}

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

// fixtureStore 播种一个空间、两个座位、两个时段与一条占用记录。
func fixtureStore(t *testing.T) *userReservationStore {
	t.Helper()
	store := &userReservationStore{memStore: newMemStore(authoritativeMeta(), true)}
	store.system[library.SpacesCollection] = []guestkit.Document{
		doc(t, "space-1", library.Space{ID: "space-1", Name: "总馆三楼", Campus: "文理学部", Building: "总图书馆", Floor: "3F", SourceRevision: "rev-1"}),
	}
	store.system[library.SeatsCollection] = []guestkit.Document{
		doc(t, "seat-a1", library.Seat{ID: "seat-a1", SpaceID: "space-1", Label: "A01", Area: "A", SourceRevision: "rev-1"}),
		doc(t, "seat-a2", library.Seat{ID: "seat-a2", SpaceID: "space-1", Label: "A02", Area: "A", SourceRevision: "rev-1"}),
	}
	store.system[library.SlotsCollection] = []guestkit.Document{
		doc(t, "slot-morning", library.Slot{ID: "slot-morning", Name: "上午", StartMinute: 8 * 60, EndMinute: 12 * 60, SourceRevision: "rev-1"}),
		doc(t, "slot-evening", library.Slot{ID: "slot-evening", Name: "晚上", StartMinute: 18 * 60, EndMinute: 22 * 60, SourceRevision: "rev-1"}),
	}
	store.system[library.OccupancyCollection] = []guestkit.Document{
		doc(t, "seat-a1-slot-morning", library.Occupancy{SeatID: "seat-a1", SlotID: "slot-morning", Date: futureDate(), SourceRevision: "rev-1"}),
	}
	return store
}

func dispatch(t *testing.T, store guestkit.Store, capabilityID string, payload any) guestkit.ResultEnvelope {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return NewDispatcher(store).Dispatch(capabilityID, body)
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

// futureDate 返回一个仍可预约的学术日期（今天 + 30 天，上海日历日）。
func futureDate() string {
	return shiftedDate(30)
}

// futureDateLater 返回比 futureDate 晚一天的学术日期（用于排序断言）。
func futureDateLater() string {
	return shiftedDate(31)
}

func shiftedDate(days int) string {
	location, err := time.LoadLocation(library.AcademicTimezone)
	if err != nil {
		location = time.FixedZone("CST", 8*60*60)
	}
	return time.Now().In(location).AddDate(0, 0, days).Format(library.AcademicDateLayout)
}

func TestSpacesListSortsAndCaps(t *testing.T) {
	store := fixtureStore(t)
	var result spacesListResult
	decodeResult(t, dispatch(t, store, capSpacesList, library.SpacesListRequest{}), &result)
	if len(result.Spaces) != 1 || result.Spaces[0].Name != "总馆三楼" {
		t.Fatalf("spaces=%+v", result.Spaces)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestSlotsSearchMarksOccupiedSeat(t *testing.T) {
	store := fixtureStore(t)
	var result slotsSearchResult
	decodeResult(t, dispatch(t, store, capSlotsSearch, library.SlotSearchRequest{SpaceID: "space-1", Date: futureDate()}), &result)
	if len(result.Slots) != 2 {
		t.Fatalf("slots=%+v", result.Slots)
	}
	morning := result.Slots[0]
	if morning.Slot.ID != "slot-morning" {
		t.Fatalf("slot order drifted: %+v", result.Slots)
	}
	if len(morning.Seats) != 2 {
		t.Fatalf("seats=%+v", morning.Seats)
	}
	if morning.Seats[0].SeatID != "seat-a1" || morning.Seats[0].Status != library.SeatReserved {
		t.Fatalf("seat-a1 should be reserved: %+v", morning.Seats[0])
	}
	if morning.Seats[1].SeatID != "seat-a2" || morning.Seats[1].Status != library.SeatAvailable {
		t.Fatalf("seat-a2 should be available: %+v", morning.Seats[1])
	}
	// slot_id 过滤。
	result = slotsSearchResult{}
	decodeResult(t, dispatch(t, store, capSlotsSearch, library.SlotSearchRequest{SpaceID: "space-1", Date: futureDate(), SlotID: "slot-evening"}), &result)
	if len(result.Slots) != 1 || result.Slots[0].Slot.ID != "slot-evening" {
		t.Fatalf("slot filter failed: %+v", result.Slots)
	}
	// 未知空间返回带内空集。
	result = slotsSearchResult{}
	decodeResult(t, dispatch(t, store, capSlotsSearch, library.SlotSearchRequest{SpaceID: "space-missing", Date: futureDate()}), &result)
	if len(result.Slots) != 0 {
		t.Fatalf("unknown space should be empty: %+v", result.Slots)
	}
}

func TestReservationLifecycleCreateCancelReplayConflict(t *testing.T) {
	store := fixtureStore(t)
	dispatcher := NewDispatcher(store)

	// 创建预约。
	created := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-morning", Date: futureDate(),
	}))
	if !created.OK {
		t.Fatalf("create: %s %s", created.Code, created.Message)
	}
	var createResult reservationResult
	if err := json.Unmarshal(created.Result, &createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.Reservation.ReservationID == "" || createResult.Reservation.Status != library.ReservationConfirmed {
		t.Fatalf("created=%+v", createResult.Reservation)
	}
	if createResult.Reservation.StartsAt == "" || createResult.Reservation.EndsAt == "" {
		t.Fatalf("reservation bounds missing: %+v", createResult.Reservation)
	}
	reservationID := createResult.Reservation.ReservationID

	// 重复预约同一座位冲突（自己的 confirmed 预约占位，带内冲突）。
	conflict := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-morning", Date: futureDate(),
	}))
	if !conflict.OK {
		t.Fatalf("double-book should be in-band: %+v", conflict)
	}
	var seatConflict guestkit.Conflict
	if err := json.Unmarshal(conflict.Result, &seatConflict); err != nil || seatConflict.Conflict != "seat_conflict" {
		t.Fatalf("seatConflict=%+v err=%v", seatConflict, err)
	}

	// 未提供 reservation 的取消按带内 not-found。
	missing := dispatcher.Dispatch(capReservationsCancel, payload(t, library.ReservationCancelRequest{ReservationID: "missing"}))
	if !missing.OK {
		t.Fatalf("missing cancel should be in-band: %+v", missing)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(missing.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}

	// 取消本人预约。
	cancelled := dispatcher.Dispatch(capReservationsCancel, payload(t, library.ReservationCancelRequest{ReservationID: reservationID}))
	if !cancelled.OK {
		t.Fatalf("cancel: %s %s", cancelled.Code, cancelled.Message)
	}
	var cancelResult reservationResult
	if err := json.Unmarshal(cancelled.Result, &cancelResult); err != nil {
		t.Fatal(err)
	}
	if cancelResult.Reservation.Status != library.ReservationCancelled || cancelResult.Reservation.CancelledAt == "" {
		t.Fatalf("cancelled=%+v", cancelResult.Reservation)
	}

	// 重复取消带内冲突。
	again := dispatcher.Dispatch(capReservationsCancel, payload(t, library.ReservationCancelRequest{ReservationID: reservationID}))
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
	dispatcher := NewDispatcher(store)
	// 配额上限 2：先创建 slot-morning（seat-a2），再创建 slot-evening（seat-a1）。
	first := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-morning", Date: futureDate(),
	}))
	if !first.OK {
		t.Fatalf("first create: %s", first.Code)
	}
	second := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a1", SlotID: "slot-evening", Date: futureDate(),
	}))
	if !second.OK {
		t.Fatalf("second create: %s", second.Code)
	}
	third := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-evening", Date: futureDate(),
	}))
	if !third.OK {
		t.Fatalf("over-quota should be in-band: %+v", third)
	}
	var conflict guestkit.Conflict
	if err := json.Unmarshal(third.Result, &conflict); err != nil || conflict.Conflict != "quota_exceeded" {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}
}

func TestReservationCreateUnknownSpaceInBandNotFound(t *testing.T) {
	store := fixtureStore(t)
	result := NewDispatcher(store).Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-missing", SeatID: "seat-a2", SlotID: "slot-morning", Date: futureDate(),
	}))
	if !result.OK {
		t.Fatalf("unknown space should be in-band: %+v", result)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(result.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}
}

func TestReservationCreateRejectsPastSlot(t *testing.T) {
	store := fixtureStore(t)
	// 已过去的学术日期 → 时段已结束，fail-closed data_expired。
	result := NewDispatcher(store).Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-morning", Date: "2020-01-01",
	}))
	if result.OK || result.Code != guestkit.CodeDataExpired {
		t.Fatalf("past slot should be data_expired: %+v", result)
	}
}

func TestReservationsMineSortsAndLists(t *testing.T) {
	store := fixtureStore(t)
	dispatcher := NewDispatcher(store)
	// 两个不同未来日期，验证列表按日期升序。
	laterDate := futureDateLater()
	if ok := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a2", SlotID: "slot-evening", Date: laterDate,
	})); !ok.OK {
		t.Fatalf("create 1: %s", ok.Code)
	}
	// 取消第一条后建立第二条，验证列表含已取消项。
	if ok := dispatcher.Dispatch(capReservationsCreate, payload(t, library.ReservationCreateRequest{
		SpaceID: "space-1", SeatID: "seat-a1", SlotID: "slot-evening", Date: futureDate(),
	})); !ok.OK {
		t.Fatalf("create 2: %s", ok.Code)
	}
	var list reservationsResult
	decodeResult(t, dispatcher.Dispatch(capReservationsMine, payload(t, library.ReservationListRequest{})), &list)
	if len(list.Reservations) != 2 {
		t.Fatalf("mine=%+v", list.Reservations)
	}
	if list.Reservations[0].Date != futureDate() || list.Reservations[0].SeatID != "seat-a1" {
		t.Fatalf("sort by date drifted: %+v", list.Reservations)
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
			store := &userReservationStore{memStore: newMemStore(tc.meta, tc.metaFound)}
			if envelope := dispatch(t, store, capSpacesList, library.SpacesListRequest{}); envelope.OK || envelope.Code != tc.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestRevisionMismatchFailsClosed(t *testing.T) {
	store := fixtureStore(t)
	// 文档 source_revision 与快照元数据不一致 → data_unavailable。
	store.system[library.SpacesCollection] = []guestkit.Document{
		doc(t, "space-1", library.Space{ID: "space-1", Name: "总馆三楼", SourceRevision: "other"}),
	}
	if envelope := dispatch(t, store, capSpacesList, library.SpacesListRequest{}); envelope.OK || envelope.Code != guestkit.CodeDataUnavailable {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestUnknownCapabilityAndInvalidPayloads(t *testing.T) {
	store := fixtureStore(t)
	if envelope := NewDispatcher(store).Dispatch("library.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
	if envelope := NewDispatcher(store).Dispatch(capSpacesList, json.RawMessage(`{"extra":1}`)); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("unknown field envelope=%+v", envelope)
	}
	if envelope := NewDispatcher(store).Dispatch(capSlotsSearch, json.RawMessage(`{"date":"2026-09-02"}`)); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("missing space_id envelope=%+v", envelope)
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
