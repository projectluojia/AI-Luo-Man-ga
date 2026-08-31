package classroom_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	classroomservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/classroom"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

func TestRegisterExposesStrictSchemasAndConfirmation(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "classroom-reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	reg := registry.New()
	if err := classroomservice.Register(reg, classroomservice.NewService(store)); err != nil {
		t.Fatal(err)
	}
	if len(reg.Services()) != 1 || len(reg.Tools()) != 6 || len(reg.Capabilities()) != 6 {
		t.Fatalf("services=%d tools=%d capabilities=%d", len(reg.Services()), len(reg.Tools()), len(reg.Capabilities()))
	}
	if err := reg.ValidateCapabilityInput(classroomservice.RoomsSearchCapabilityID, []byte(`{"date":"2026-08-31","campus_id":"campus-wenli","period":3}`)); err != nil {
		t.Fatalf("valid search rejected: %v", err)
	}
	if err := reg.ValidateCapabilityInput(classroomservice.RoomsSearchCapabilityID, []byte(`{"date":"2026-08-31","campus_id":"campus-wenli","period":3,"unexpected":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown field err=%v", err)
	}
	create, _, err := reg.ResolveCapability(classroomservice.ScheduleCreateCapabilityID)
	if err != nil || !create.RequiresConfirmation || create.SideEffect != registry.SideEffectWrite {
		t.Fatalf("create spec=%#v err=%v", create, err)
	}
	cancel, _, err := reg.ResolveCapability(classroomservice.ScheduleCancelCapabilityID)
	if err != nil || !cancel.RequiresConfirmation || cancel.SideEffect != registry.SideEffectWrite {
		t.Fatalf("cancel spec=%#v err=%v", cancel, err)
	}
}

func TestClassroomCapabilitiesGovernSnapshotAndFilters(t *testing.T) {
	harness := newClassroomHarness(t)
	defer harness.close()
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)

	if _, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","period":1}`, "", false); !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("missing snapshot err=%v", err)
	}

	if err := demo.LoadClassroomData(context.Background(), harness.store, now); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","period":1}`, "", false); !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("demo snapshot err=%v, want untrusted", err)
	}
	if publicerror.Capability(contracts.ErrDataUntrusted).Code != "data_non_authoritative" {
		t.Fatalf("public error=%#v", publicerror.Capability(contracts.ErrDataUntrusted))
	}

	expired := testSnapshot(now)
	expired.Authoritative = true
	expired.ImportedAt = now.Add(-2 * time.Hour)
	expired.ValidUntil = now.Add(-time.Minute)
	if err := harness.store.ReplaceClassroomSnapshot(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","period":1}`, "", false); !errors.Is(err, contracts.ErrDataExpired) {
		t.Fatalf("expired err=%v", err)
	}

	if err := harness.store.ReplaceClassroomSnapshot(context.Background(), testSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	search, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","period":1}`, "", false)
	if err != nil {
		t.Fatal(err)
	}
	rooms := asList(t, search["rooms"])
	if len(rooms) == 0 {
		t.Fatalf("authoritative search empty: %v", search)
	}
	if status, _ := search["data_status"].(map[string]any); status["state"] != classroom.DataStateAuthoritativeFresh || status["authoritative"] != true {
		t.Fatalf("data_status=%v", search["data_status"])
	}

	filtered, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","building_id":"bld-wenli-yijiao","period":1}`, "", false)
	if err != nil {
		t.Fatal(err)
	}
	filteredRooms := asList(t, filtered["rooms"])
	if len(filteredRooms) != 1 || filteredRooms[0].(map[string]any)["id"] != "room-wenli-yijiao-101" {
		t.Fatalf("building filter=%v", filteredRooms)
	}

	empty, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-gongxue","building_id":"missing-building","period":13}`, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if rooms := asList(t, empty["rooms"]); rooms == nil || len(rooms) != 0 {
		t.Fatalf("empty result=%v, want []", empty["rooms"])
	}

	campuses, err := harness.invoke(classroomservice.CampusesListCapabilityID, `{}`, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(asList(t, campuses["campuses"])) != 4 {
		t.Fatalf("campuses=%v", campuses["campuses"])
	}
	buildings, err := harness.invoke(classroomservice.BuildingsListCapabilityID, `{"campus_id":"campus-xinxi"}`, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(asList(t, buildings["buildings"])) != 1 {
		t.Fatalf("buildings=%v", buildings["buildings"])
	}

	if _, err := harness.invoke(classroomservice.RoomsSearchCapabilityID, `{"date":"2026-08-31","campus_id":"campus-wenli","period":1,"extra":1}`, "", false); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown field err=%v", err)
	}
}

func TestClassroomScheduleRequiresUserConfirmationAndIsIdempotent(t *testing.T) {
	harness := newClassroomHarness(t)
	defer harness.close()
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)
	if err := harness.store.ReplaceClassroomSnapshot(context.Background(), testSnapshot(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.NewService(harness.store).CreateUser(context.Background(), "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.NewService(harness.store).CreateUser(context.Background(), "user-b"); err != nil {
		t.Fatal(err)
	}

	if _, err := harness.invoke(classroomservice.ScheduleCreateCapabilityID, `{"room_id":"room-wenli-jiao5-102","date":"2026-08-31","period":5}`, "user-a", false); !errors.Is(err, runtime.ErrConfirmationRequired) {
		t.Fatalf("create without confirmation err=%v", err)
	}
	created, err := harness.invoke(classroomservice.ScheduleCreateCapabilityID, `{"schedule_id":"sched-1","room_id":"room-wenli-jiao5-102","date":"2026-08-31","period":5,"title":"自习"}`, "user-a", true)
	if err != nil {
		t.Fatal(err)
	}
	schedule := created["schedule"].(map[string]any)
	if schedule["schedule_id"] != "sched-1" || schedule["status"] != classroom.ScheduleStatusScheduled {
		t.Fatalf("created=%v", created)
	}
	replay, err := harness.invoke(classroomservice.ScheduleCreateCapabilityID, `{"schedule_id":"sched-1","room_id":"room-wenli-jiao5-102","date":"2026-08-31","period":5,"title":"自习"}`, "user-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if replay["schedule"].(map[string]any)["schedule_id"] != "sched-1" {
		t.Fatalf("replay=%v", replay)
	}
	if _, err := harness.invoke(classroomservice.ScheduleCreateCapabilityID, `{"room_id":"room-wenli-jiao5-102","date":"2026-08-31","period":5}`, "", true); !errors.Is(err, classroom.ErrUserRequired) && !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("anonymous create err=%v", err)
	}
	listed, err := harness.invoke(classroomservice.ScheduleListCapabilityID, `{}`, "user-a", false)
	if err != nil || len(asList(t, listed["schedules"])) != 1 {
		t.Fatalf("list=%v err=%v", listed, err)
	}
	otherUser, err := harness.invoke(classroomservice.ScheduleListCapabilityID, `{}`, "user-b", false)
	if err != nil || len(asList(t, otherUser["schedules"])) != 0 {
		t.Fatalf("cross-user list=%v err=%v", otherUser, err)
	}
	if _, err := harness.invoke(classroomservice.ScheduleCancelCapabilityID, `{"schedule_id":"sched-1"}`, "user-a", false); !errors.Is(err, runtime.ErrConfirmationRequired) {
		t.Fatalf("cancel without confirmation err=%v", err)
	}
	cancelled, err := harness.invoke(classroomservice.ScheduleCancelCapabilityID, `{"schedule_id":"sched-1"}`, "user-a", true)
	if err != nil || cancelled["schedule"].(map[string]any)["status"] != classroom.ScheduleStatusCancelled {
		t.Fatalf("cancel=%v err=%v", cancelled, err)
	}
	replayCancel, err := harness.invoke(classroomservice.ScheduleCancelCapabilityID, `{"schedule_id":"sched-1"}`, "user-a", true)
	if err != nil || replayCancel["schedule"].(map[string]any)["status"] != classroom.ScheduleStatusCancelled {
		t.Fatalf("idempotent cancel=%v err=%v", replayCancel, err)
	}
	if _, err := harness.invoke(classroomservice.ScheduleCreateCapabilityID, `{"schedule_id":"sched-1","room_id":"room-wenli-jiao5-102","date":"2026-08-31","period":5}`, "user-a", true); !errors.Is(err, classroom.ErrIllegalState) {
		t.Fatalf("revive err=%v", err)
	}

	deadline, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := harness.dispatcher.InvokeCapability(deadline, harness.request("user-a", true), classroomservice.ScheduleListCapabilityID, json.RawMessage(`{}`)); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, contracts.ErrDeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := harness.dispatcher.InvokeCapability(canceled, harness.request("user-a", true), classroomservice.ScheduleListCapabilityID, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

type classroomHarness struct {
	t          *testing.T
	dir        string
	store      *sqlite.Store
	dispatcher *runtime.Dispatcher
}

func newClassroomHarness(t *testing.T) *classroomHarness {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "classroom-cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := classroomservice.Register(reg, classroomservice.NewService(store)); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	for _, id := range classroomservice.CapabilityIDs() {
		policy.Enable("campus-services", id)
		policy.Enable("other-app", id)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: confirmOK{},
	})
	return &classroomHarness{t: t, dir: dir, store: store, dispatcher: dispatcher}
}

func (h *classroomHarness) close() {
	sqlitetest.CloseAndWait(h.t, h.store, h.dir)
}

func (h *classroomHarness) request(userID string, confirm bool) contracts.RequestContext {
	h.t.Helper()
	request := contracts.RequestContext{
		AppID: "campus-services", EchoID: "echo", RequestID: "req-" + time.Now().Format("150405.000000000"),
		UserID: userID, Deadline: time.Now().Add(time.Minute),
		IdempotencyKey: "idem-" + time.Now().Format("150405.000000000"),
	}
	if confirm {
		request.ConfirmationID = "confirmation-1"
	}
	return request
}

func (h *classroomHarness) invoke(capabilityID, payload, userID string, confirm bool) (map[string]any, error) {
	h.t.Helper()
	raw, err := h.dispatcher.InvokeCapability(context.Background(), h.request(userID, confirm), capabilityID, json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		h.t.Fatal(err)
	}
	return decoded, nil
}

type confirmOK struct{}

func (confirmOK) VerifyConfirmation(context.Context, runtime.ConfirmationRequest) error { return nil }

func asList(t *testing.T, value any) []any {
	t.Helper()
	if value == nil {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("value=%T %v, want array", value, value)
	}
	return list
}

func testSnapshot(now time.Time) sqlite.ClassroomSnapshot {
	revision := "rev-auth"
	return sqlite.ClassroomSnapshot{
		AppID: "campus-services", Revision: revision, Source: "test-authoritative-fixture",
		Authoritative: true, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		Campuses: []classroom.Campus{
			{ID: "campus-wenli", Name: "文理学部"},
			{ID: "campus-xinxi", Name: "信息学部"},
			{ID: "campus-gongxue", Name: "工学部"},
			{ID: "campus-yixue", Name: "医学部"},
		},
		Buildings: []classroom.Building{
			{ID: "bld-wenli-jiao5", CampusID: "campus-wenli", Name: "教五"},
			{ID: "bld-wenli-yijiao", CampusID: "campus-wenli", Name: "一教"},
			{ID: "bld-xinxi-jisuanji", CampusID: "campus-xinxi", Name: "计算机大楼"},
			{ID: "bld-gongxue-gongjiao", CampusID: "campus-gongxue", Name: "工教"},
			{ID: "bld-yixue-yixue", CampusID: "campus-yixue", Name: "医学楼"},
		},
		Rooms: []classroom.Room{
			{ID: "room-wenli-jiao5-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-101", Type: "多媒体教室", Capacity: 120, Floor: "3"},
			{ID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-102", Type: "普通教室", Capacity: 80, Floor: "3"},
			{ID: "room-wenli-yijiao-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-yijiao", Name: "一教-101", Type: "多媒体教室", Capacity: 200, Floor: "1"},
			{ID: "room-xinxi-jisuanji-201", CampusID: "campus-xinxi", BuildingID: "bld-xinxi-jisuanji", Name: "计算机大楼-201", Type: "机房", Capacity: 90, Floor: "2"},
			{ID: "room-gongxue-gongjiao-301", CampusID: "campus-gongxue", BuildingID: "bld-gongxue-gongjiao", Name: "工教-301", Type: "普通教室", Capacity: 100, Floor: "3"},
			{ID: "room-yixue-yixue-401", CampusID: "campus-yixue", BuildingID: "bld-yixue-yixue", Name: "医学楼-401", Type: "多媒体教室", Capacity: 80, Floor: "4"},
		},
		Occupancy: []classroom.Occupancy{
			{RoomID: "room-wenli-jiao5-101", AcademicDate: "2026-08-31", Period: 1},
		},
	}
}
