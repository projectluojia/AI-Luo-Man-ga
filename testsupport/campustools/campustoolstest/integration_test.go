//go:build integration

// 教室 + 学业校历 hosted 包端到端：真实 ailuo.toml 清单 + 真实 guest 源码现场
// 编译 + packstore 快照与个人作用域日程。覆盖查询治理、日程生命周期
// （幂等 + 确认门槛）、校历窗口查询与非权威快照拒绝，全部经真实 Dispatcher。
package campustoolstest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campustools/campustoolstest"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// seedClassroom 播种教室系统快照：campus-a 两个学部楼栋、两间教室，room-1
// 在 2026-09-07 第 1 节被占用。AppID 与 invoke 的治理上下文一致（packstore
// 宿主函数从上下文注入 AppID）。
func seedClassroom(t *testing.T, store packstore.Store) {
	t.Helper()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: campustoolstest.ClassroomPackageID, PackageID: campustoolstest.ClassroomPackageID, Namespace: campustoolstest.ClassroomStorageNamespace}
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"campuses":  {hostedtest.MustDoc(t, "campus-a", map[string]any{"id": "campus-a", "name": "文理学部", "source_revision": "rev-1"})},
		"buildings": {hostedtest.MustDoc(t, "b1", map[string]any{"id": "b1", "campus_id": "campus-a", "name": "教五", "source_revision": "rev-1"}), hostedtest.MustDoc(t, "b2", map[string]any{"id": "b2", "campus_id": "campus-a", "name": "一教", "source_revision": "rev-1"})},
		"rooms":     {hostedtest.MustDoc(t, "room-1", map[string]any{"id": "room-1", "campus_id": "campus-a", "building_id": "b1", "name": "教五-101", "source_revision": "rev-1"}), hostedtest.MustDoc(t, "room-2", map[string]any{"id": "room-2", "campus_id": "campus-a", "building_id": "b2", "name": "一教-101", "source_revision": "rev-1"})},
		"occupancy": {hostedtest.MustDoc(t, "room-1-2026-09-07-1", map[string]any{"room_id": "room-1", "academic_date": "2026-09-07", "period": 1, "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
}

// seedCalendar 播种校历系统快照：一个开学事件。
func seedCalendar(t *testing.T, store packstore.Store) {
	t.Helper()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: campustoolstest.CalendarPackageID, PackageID: campustoolstest.CalendarPackageID, Namespace: campustoolstest.CalendarStorageNamespace}
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"events": {hostedtest.MustDoc(t, "e1", map[string]any{"id": "e1", "title": "开学", "type": "term", "start_at": "2026-09-02T00:00:00Z", "end_at": "2026-09-03T00:00:00Z", "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
}

// newClassroomDispatcher 装配教室包完整治理链路 Dispatcher。
func newClassroomDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	seedClassroom(t, store)
	campustoolstest.RegisterClassroomHosted(t, reg, store)
	return hostedtest.NewDispatcher(t, reg, campustoolstest.ClassroomPackageID, campustoolstest.ClassroomCapabilityIDs())
}

// newCalendarDispatcher 装配校历包完整治理链路 Dispatcher。
func newCalendarDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	seedCalendar(t, store)
	campustoolstest.RegisterCalendarHosted(t, reg, store)
	return hostedtest.NewDispatcher(t, reg, campustoolstest.CalendarPackageID, []string{campustoolstest.CalendarEventsListCapabilityID})
}

func invoke(t *testing.T, d *runtime.Dispatcher, appID, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, appID, capabilityID, payload, idempotencyKey, confirmationID)
}

// TestHostedClassroomRoomsSearch 经真实 wasm guest 查询空闲教室：排除占用
// （2026-09-07 第 1 节 room-1 被占用）、治理状态 authoritative_fresh。
func TestHostedClassroomRoomsSearch(t *testing.T) {
	d := newClassroomDispatcher(t)
	ok, result, errText := invoke(t, d, campustoolstest.ClassroomPackageID, campustoolstest.ClassroomRoomsSearchCapabilityID,
		`{"date":"2026-09-07","campus_id":"campus-a","period":1}`, "", "")
	if !ok {
		t.Fatalf("rooms.search failed: %s", errText)
	}
	var decoded struct {
		DataStatus struct {
			State string `json:"state"`
		} `json:"data_status"`
		Rooms []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			BuildingID string `json:"building_id"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("rooms.search result %s: %v", result, err)
	}
	if decoded.DataStatus.State != "authoritative_fresh" {
		t.Fatalf("data_status = %+v", decoded.DataStatus)
	}
	if len(decoded.Rooms) != 1 || decoded.Rooms[0].ID != "room-2" || decoded.Rooms[0].BuildingID != "b2" {
		t.Fatalf("rooms = %#v", decoded.Rooms)
	}
}

// TestHostedClassroomScheduleLifecycle 经真实 wasm guest 走通日程生命周期：
// 创建（快照内校验 room、解析楼栋名）→ 列表（个人作用域注入 UserID）→
// 取消（确认门槛）→ 重复取消带内冲突。
func TestHostedClassroomScheduleLifecycle(t *testing.T) {
	d := newClassroomDispatcher(t)
	appID := campustoolstest.ClassroomPackageID

	ok, result, errText := invoke(t, d, appID, campustoolstest.ClassroomScheduleCreateCapabilityID,
		`{"room_id":"room-2","date":"2026-09-08","period":3}`, "idem-create", "confirm-create")
	if !ok {
		t.Fatalf("schedule.create failed: %s", errText)
	}
	var created struct {
		Schedule struct {
			ScheduleID string `json:"schedule_id"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			RoomName   string `json:"room_name"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}
	if created.Schedule.ScheduleID == "" || created.Schedule.Status != "scheduled" || created.Schedule.RoomName != "一教-101" || created.Schedule.Title == "" {
		t.Fatalf("created = %#v", created.Schedule)
	}
	scheduleID := created.Schedule.ScheduleID

	// 列出日程（个人作用域注入 UserID）。
	ok, result, errText = invoke(t, d, appID, campustoolstest.ClassroomScheduleListCapabilityID, `{}`, "", "")
	if !ok {
		t.Fatalf("schedule.list failed: %s", errText)
	}
	var list struct {
		Schedules []struct {
			ScheduleID string `json:"schedule_id"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatalf("list result %s: %v", result, err)
	}
	if len(list.Schedules) != 1 || list.Schedules[0].ScheduleID != scheduleID {
		t.Fatalf("listed = %#v", list.Schedules)
	}

	// 取消是确认门槛能力：无确认被 dispatcher 前置拒绝。
	ok, _, errText = invoke(t, d, appID, campustoolstest.ClassroomScheduleCancelCapabilityID,
		`{"schedule_id":"`+scheduleID+`"}`, "idem-cancel", "")
	if ok {
		t.Fatal("cancel without confirmation should fail")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("cancel error = %q, want confirmation required", errText)
	}

	ok, result, errText = invoke(t, d, appID, campustoolstest.ClassroomScheduleCancelCapabilityID,
		`{"schedule_id":"`+scheduleID+`"}`, "idem-cancel", "confirm-cancel")
	if !ok {
		t.Fatalf("schedule.cancel failed: %s", errText)
	}
	var cancelled struct {
		Schedule struct {
			Status      string `json:"status"`
			CancelledAt string `json:"cancelled_at"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(result, &cancelled); err != nil {
		t.Fatalf("cancel result %s: %v", result, err)
	}
	if cancelled.Schedule.Status != "cancelled" || cancelled.Schedule.CancelledAt == "" {
		t.Fatalf("cancelled = %#v", cancelled.Schedule)
	}

	// 重复取消应答带内冲突。
	ok, result, errText = invoke(t, d, appID, campustoolstest.ClassroomScheduleCancelCapabilityID,
		`{"schedule_id":"`+scheduleID+`"}`, "idem-cancel-2", "confirm-cancel")
	if !ok {
		t.Fatalf("double cancel failed: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil {
		t.Fatalf("conflict result %s: %v", result, err)
	}
	if conflict.Conflict == "" {
		t.Fatalf("double cancel not marked conflict: %s", result)
	}
}

// TestHostedClassroomCreateUnknownRoom 带内 not-found：room 不存在时创建
// 返回 found=false 而非错误。
func TestHostedClassroomCreateUnknownRoom(t *testing.T) {
	d := newClassroomDispatcher(t)
	ok, result, errText := invoke(t, d, campustoolstest.ClassroomPackageID, campustoolstest.ClassroomScheduleCreateCapabilityID,
		`{"room_id":"room-missing","date":"2026-09-08","period":3}`, "idem-create-missing", "confirm-create")
	if !ok {
		t.Fatalf("schedule.create failed: %s", errText)
	}
	var decoded struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}
	if decoded.Found {
		t.Fatalf("unknown room should be found=false: %s", result)
	}
}

// TestHostedClassroomGovernedSnapshotRejection 非权威快照经 guest 侧治理以
// 稳定错误码拒绝：guest 信封 data_untrusted 在内核错误面投影为
// data_non_authoritative（细节不外泄）。
func TestHostedClassroomGovernedSnapshotRejection(t *testing.T) {
	reg := registry.New()
	store := hostedtest.MemoryStore()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: campustoolstest.ClassroomPackageID, PackageID: campustoolstest.ClassroomPackageID, Namespace: campustoolstest.ClassroomStorageNamespace}
	demo := hostedtest.AuthoritativeMeta(now)
	demo.Authoritative = false
	demo.Source = "demo-fixture"
	if err := store.ReplaceSnapshot(context.Background(), scope, demo, map[string][]packstore.Document{
		"rooms": {hostedtest.MustDoc(t, "room-1", map[string]any{"id": "room-1", "campus_id": "c", "name": "r", "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
	campustoolstest.RegisterClassroomHosted(t, reg, store)
	d := hostedtest.NewDispatcher(t, reg, campustoolstest.ClassroomPackageID, campustoolstest.ClassroomCapabilityIDs())
	request := hostedtest.RequestContext(campustoolstest.ClassroomPackageID, "request-governed-rejection")
	_, invokeErr := d.InvokeCapability(t.Context(), request, campustoolstest.ClassroomRoomsSearchCapabilityID, json.RawMessage(`{"date":"2026-09-07","campus_id":"c","period":1}`))
	if invokeErr == nil || !strings.Contains(invokeErr.Error(), "hosted package rejected the call") {
		t.Fatalf("err = %v", invokeErr)
	}
	var invocation loader.InvocationError
	if !errors.As(invokeErr, &invocation) || invocation.Code != "data_non_authoritative" {
		t.Fatalf("invocation error = %#v, want data_non_authoritative", invocation)
	}
}

// TestHostedCalendarEventsList 经真实 wasm guest 按窗口查询校历事件。
func TestHostedCalendarEventsList(t *testing.T) {
	d := newCalendarDispatcher(t)
	ok, result, errText := invoke(t, d, campustoolstest.CalendarPackageID, campustoolstest.CalendarEventsListCapabilityID,
		`{"from":"2026-09-01T00:00:00Z","to":"2026-09-30T00:00:00Z"}`, "", "")
	if !ok {
		t.Fatalf("events.list failed: %s", errText)
	}
	var decoded struct {
		Events []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"events"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("events.list result %s: %v", result, err)
	}
	if len(decoded.Events) != 1 || decoded.Events[0].Title != "开学" {
		t.Fatalf("events = %#v", decoded.Events)
	}
}
