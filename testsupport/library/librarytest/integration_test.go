//go:build integration

// 图书馆 hosted 包端到端：真实 ailuo.toml 清单 + 真实 guest 源码现场编译 +
// packstore 目录快照（系统作用域）与个人预约（用户作用域）。覆盖目录查询治理、
// 预约生命周期（幂等 + 确认门槛 + 配额 + 占用冲突）与非权威快照拒绝，全部经
// 真实 Dispatcher。
package librarytest_test

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
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/library/librarytest"
)

func seedLibrary(t *testing.T, store packstore.Store) {
	t.Helper()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: librarytest.PackageID, PackageID: librarytest.PackageID, Namespace: librarytest.StorageNamespace}
	seatDate := futureDate(30)
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"spaces": {hostedtest.MustDoc(t, "space-1", map[string]any{"id": "space-1", "name": "总馆三楼", "campus": "文理学部", "building": "总图书馆", "floor": "3F", "source_revision": "rev-1"})},
		"seats": {
			hostedtest.MustDoc(t, "seat-a1", map[string]any{"id": "seat-a1", "space_id": "space-1", "label": "A01", "area": "A", "source_revision": "rev-1"}),
			hostedtest.MustDoc(t, "seat-a2", map[string]any{"id": "seat-a2", "space_id": "space-1", "label": "A02", "area": "A", "source_revision": "rev-1"}),
		},
		"slots": {
			hostedtest.MustDoc(t, "slot-morning", map[string]any{"id": "slot-morning", "name": "上午", "start_minute": 480, "end_minute": 720, "source_revision": "rev-1"}),
			hostedtest.MustDoc(t, "slot-evening", map[string]any{"id": "slot-evening", "name": "晚上", "start_minute": 1080, "end_minute": 1320, "source_revision": "rev-1"}),
		},
		"occupancy": {hostedtest.MustDoc(t, "occ-1", map[string]any{"seat_id": "seat-a1", "slot_id": "slot-morning", "date": seatDate, "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
}

func newLibraryDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	seedLibrary(t, store)
	librarytest.RegisterHosted(t, reg, store)
	return hostedtest.NewDispatcher(t, reg, librarytest.PackageID, librarytest.CapabilityIDs())
}

func invoke(t *testing.T, d *runtime.Dispatcher, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, librarytest.PackageID, capabilityID, payload, idempotencyKey, confirmationID)
}

// futureDate 返回 days 天后的学术日期（上海日历日）。
func futureDate(days int) string {
	return time.Now().In(time.FixedZone("CST", 8*60*60)).AddDate(0, 0, days).Format("2006-01-02")
}

// TestHostedLibrarySpacesList 经真实 wasm guest 查询空间目录：治理状态
// authoritative_fresh。
func TestHostedLibrarySpacesList(t *testing.T) {
	d := newLibraryDispatcher(t)
	ok, result, errText := invoke(t, d, librarytest.SpacesListCapabilityID, `{}`, "", "")
	if !ok {
		t.Fatalf("spaces.list failed: %s", errText)
	}
	var decoded struct {
		DataStatus struct {
			State string `json:"state"`
		} `json:"data_status"`
		Spaces []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("spaces.list result %s: %v", result, err)
	}
	if decoded.DataStatus.State != "authoritative_fresh" {
		t.Fatalf("data_status = %+v", decoded.DataStatus)
	}
	if len(decoded.Spaces) != 1 || decoded.Spaces[0].ID != "space-1" || decoded.Spaces[0].Name != "总馆三楼" {
		t.Fatalf("spaces = %#v", decoded.Spaces)
	}
}

// TestHostedLibrarySlotsSearch 经真实 wasm guest 查询座位时段：占用座位标记
// reserved，空闲座位 available。
func TestHostedLibrarySlotsSearch(t *testing.T) {
	d := newLibraryDispatcher(t)
	ok, result, errText := invoke(t, d, librarytest.SlotsSearchCapabilityID,
		`{"space_id":"space-1","date":"`+futureDate(30)+`"}`, "", "")
	if !ok {
		t.Fatalf("slots.search failed: %s", errText)
	}
	var decoded struct {
		Slots []struct {
			Slot struct {
				ID string `json:"id"`
			} `json:"slot"`
			Seats []struct {
				SeatID string `json:"seat_id"`
				Status string `json:"status"`
			} `json:"seats"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("slots.search result %s: %v", result, err)
	}
	if len(decoded.Slots) != 2 {
		t.Fatalf("slots = %#v", decoded.Slots)
	}
	// 按时段 ID 定位（快照序不保证时段顺序）。
	var morning *struct {
		Slot struct {
			ID string `json:"id"`
		} `json:"slot"`
		Seats []struct {
			SeatID string `json:"seat_id"`
			Status string `json:"status"`
		} `json:"seats"`
	}
	for index := range decoded.Slots {
		if decoded.Slots[index].Slot.ID == "slot-morning" {
			morning = &decoded.Slots[index]
		}
	}
	if morning == nil || len(morning.Seats) != 2 {
		t.Fatalf("slot-morning = %#v", decoded.Slots)
	}
	if morning.Seats[0].SeatID != "seat-a1" || morning.Seats[0].Status != "reserved" {
		t.Fatalf("seat-a1 should be reserved: %#v", morning.Seats[0])
	}
	if morning.Seats[1].SeatID != "seat-a2" || morning.Seats[1].Status != "available" {
		t.Fatalf("seat-a2 should be available: %#v", morning.Seats[1])
	}
}

// TestHostedLibraryReservationLifecycle 经真实 wasm guest 走通预约生命周期：
// 创建 → 重复预约带内冲突 → 取消（确认门槛）→ 重复取消带内冲突。
func TestHostedLibraryReservationLifecycle(t *testing.T) {
	d := newLibraryDispatcher(t)
	date := futureDate(30)

	ok, result, errText := invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a2","slot_id":"slot-morning","date":"`+date+`"}`, "idem-create", "confirm-create")
	if !ok {
		t.Fatalf("reservations.create failed: %s", errText)
	}
	var created struct {
		Reservation struct {
			ReservationID string `json:"reservation_id"`
			Status        string `json:"status"`
			SeatLabel     string `json:"seat_label"`
		} `json:"reservation"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}
	if created.Reservation.ReservationID == "" || created.Reservation.Status != "confirmed" || created.Reservation.SeatLabel != "A02" {
		t.Fatalf("created = %#v", created.Reservation)
	}
	reservationID := created.Reservation.ReservationID

	// 重复预约同一座位带内冲突。
	ok, result, errText = invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a2","slot_id":"slot-morning","date":"`+date+`"}`, "idem-conflict", "confirm-create")
	if !ok {
		t.Fatalf("double-book failed: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil {
		t.Fatalf("conflict result %s: %v", result, err)
	}
	if conflict.Conflict != "seat_conflict" {
		t.Fatalf("conflict = %q: %s", conflict.Conflict, result)
	}

	// 我的预约列表含已创建项。
	ok, result, errText = invoke(t, d, librarytest.ReservationsMineCapabilityID, `{}`, "", "")
	if !ok {
		t.Fatalf("reservations.mine failed: %s", errText)
	}
	var mine struct {
		Reservations []struct {
			ReservationID string `json:"reservation_id"`
		} `json:"reservations"`
	}
	if err := json.Unmarshal(result, &mine); err != nil {
		t.Fatalf("mine result %s: %v", result, err)
	}
	if len(mine.Reservations) != 1 || mine.Reservations[0].ReservationID != reservationID {
		t.Fatalf("mine = %#v", mine.Reservations)
	}

	// 取消是确认门槛能力：无确认被 dispatcher 前置拒绝。
	ok, _, errText = invoke(t, d, librarytest.ReservationsCancelCapabilityID,
		`{"reservation_id":"`+reservationID+`"}`, "idem-cancel", "")
	if ok {
		t.Fatal("cancel without confirmation should fail")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("cancel error = %q, want confirmation required", errText)
	}

	ok, result, errText = invoke(t, d, librarytest.ReservationsCancelCapabilityID,
		`{"reservation_id":"`+reservationID+`"}`, "idem-cancel", "confirm-cancel")
	if !ok {
		t.Fatalf("reservations.cancel failed: %s", errText)
	}
	var cancelled struct {
		Reservation struct {
			Status string `json:"status"`
		} `json:"reservation"`
	}
	if err := json.Unmarshal(result, &cancelled); err != nil {
		t.Fatalf("cancel result %s: %v", result, err)
	}
	if cancelled.Reservation.Status != "cancelled" {
		t.Fatalf("cancelled = %#v", cancelled.Reservation)
	}

	// 重复取消带内冲突。
	ok, result, errText = invoke(t, d, librarytest.ReservationsCancelCapabilityID,
		`{"reservation_id":"`+reservationID+`"}`, "idem-cancel-2", "confirm-cancel")
	if !ok {
		t.Fatalf("double cancel failed: %s", errText)
	}
	var cancelConflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &cancelConflict); err != nil || cancelConflict.Conflict == "" {
		t.Fatalf("double cancel not marked conflict: %s err=%v", result, err)
	}
}

// TestHostedLibraryReservationQuota 经真实 wasm guest 验证活跃预约配额：
// 超出上限按带内 quota_exceeded 冲突应答。
func TestHostedLibraryReservationQuota(t *testing.T) {
	d := newLibraryDispatcher(t)
	date := futureDate(30)
	// 配额上限 2：slot-morning × seat-a2 与 slot-evening × seat-a1。
	if ok, _, errText := invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a2","slot_id":"slot-morning","date":"`+date+`"}`, "idem-first", "confirm-first"); !ok {
		t.Fatalf("first create failed: %s", errText)
	}
	if ok, _, errText := invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a1","slot_id":"slot-evening","date":"`+date+`"}`, "idem-second", "confirm-second"); !ok {
		t.Fatalf("second create failed: %s", errText)
	}
	ok, result, errText := invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a2","slot_id":"slot-evening","date":"`+date+`"}`, "idem-third", "confirm-third")
	if !ok {
		t.Fatalf("over-quota should be in-band: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil || conflict.Conflict != "quota_exceeded" {
		t.Fatalf("conflict = %#v err=%v", conflict, err)
	}
}

// TestHostedLibraryCreatePastSlotFailClosed 过期时段创建按 data_expired 稳定
// 错误码拒绝（fail-closed；错误码只进 InvocationError，消息不外泄）。
func TestHostedLibraryCreatePastSlotFailClosed(t *testing.T) {
	d := newLibraryDispatcher(t)
	_, _, errText := invoke(t, d, librarytest.ReservationsCreateCapabilityID,
		`{"space_id":"space-1","seat_id":"seat-a2","slot_id":"slot-morning","date":"2020-01-01"}`, "idem-past", "confirm-past")
	if errText == "" {
		t.Fatal("past slot create should fail")
	}
	if !strings.Contains(errText, "hosted package rejected the call") {
		t.Fatalf("error = %q, want hosted rejection", errText)
	}
}

// TestHostedLibraryGovernedSnapshotRejection 非权威快照经 guest 侧治理以稳定
// 错误码拒绝：guest 信封 data_untrusted 在内核错误面投影为
// data_non_authoritative（细节不外泄）。
func TestHostedLibraryGovernedSnapshotRejection(t *testing.T) {
	reg := registry.New()
	store := hostedtest.MemoryStore()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: librarytest.PackageID, PackageID: librarytest.PackageID, Namespace: librarytest.StorageNamespace}
	demo := hostedtest.AuthoritativeMeta(now)
	demo.Authoritative = false
	demo.Source = "demo-fixture"
	if err := store.ReplaceSnapshot(context.Background(), scope, demo, map[string][]packstore.Document{
		"spaces": {hostedtest.MustDoc(t, "space-1", map[string]any{"id": "space-1", "name": "总馆三楼", "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
	librarytest.RegisterHosted(t, reg, store)
	d := hostedtest.NewDispatcher(t, reg, librarytest.PackageID, librarytest.CapabilityIDs())
	request := hostedtest.RequestContext(librarytest.PackageID, "request-governed-rejection")
	_, invokeErr := d.InvokeCapability(t.Context(), request, librarytest.SpacesListCapabilityID, json.RawMessage(`{}`))
	if invokeErr == nil || !strings.Contains(invokeErr.Error(), "hosted package rejected the call") {
		t.Fatalf("err = %v", invokeErr)
	}
	var invocation loader.InvocationError
	if !errors.As(invokeErr, &invocation) || invocation.Code != "data_non_authoritative" {
		t.Fatalf("invocation error = %#v, want data_non_authoritative", invocation)
	}
}
