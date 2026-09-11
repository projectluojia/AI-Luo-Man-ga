//go:build integration

// 运动场馆 hosted 包端到端：真实 ailuo.toml 清单 + 真实 guest 源码现场编译 +
// packstore 目录快照（系统作用域）与个人预约/日程（用户作用域）。覆盖目录查询
// 治理、预约生命周期（确认门槛 + 配额 + 占用冲突 + WebView 描述符）与非权威
// 快照拒绝，全部经真实 Dispatcher。
package sportstest_test

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
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/sports/sportstest"
)

// seedSports 播种场馆/项目/时段/WebView 描述符快照。时段定位于未来学术日
// （上海日历日），remaining_quota 由导入方维护。
func seedSports(t *testing.T, store packstore.Store) {
	t.Helper()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: sportstest.PackageID, PackageID: sportstest.PackageID, Namespace: sportstest.StorageNamespace}
	slotStart := time.Now().UTC().AddDate(0, 0, 1).Truncate(time.Hour)
	slotDate := slotStart.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"venues":   {hostedtest.MustDoc(t, "venue-1", map[string]any{"id": "venue-1", "name": "卓尔体育馆", "campus": "文理学部", "address": "工学部大道", "source_revision": "rev-1"})},
		"projects": {hostedtest.MustDoc(t, "project-1", map[string]any{"id": "project-1", "venue_id": "venue-1", "name": "羽毛球", "source_revision": "rev-1"})},
		"slots": {hostedtest.MustDoc(t, "slot-1", map[string]any{
			"id": "slot-1", "venue_id": "venue-1", "project_id": "project-1", "date": slotDate,
			"start_at": slotStart.Format(time.RFC3339), "end_at": slotStart.Add(2 * time.Hour).Format(time.RFC3339),
			"capacity": 4, "remaining_quota": 4, "source_revision": "rev-1",
		})},
		"webview": {hostedtest.MustDoc(t, "descriptor", map[string]any{
			"entry_url": "https://orders.example.com/sports", "required_user_agent": "LuoJia/1.0",
			"required_headers": []map[string]any{{"name": "X-App-Id", "purpose": "应用标识"}},
			"source_revision":  "rev-1",
		})},
	}); err != nil {
		t.Fatal(err)
	}
}

func newSportsDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	seedSports(t, store)
	sportstest.RegisterHosted(t, reg, store)
	return hostedtest.NewDispatcher(t, reg, sportstest.PackageID, sportstest.CapabilityIDs())
}

func invoke(t *testing.T, d *runtime.Dispatcher, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, sportstest.PackageID, capabilityID, payload, idempotencyKey, confirmationID)
}

// TestHostedSportsCatalogQueries 经真实 wasm guest 走通目录查询三件套：场馆/
// 项目/时段的治理状态均为 authoritative_fresh，时段搜索按日期过滤。
func TestHostedSportsCatalogQueries(t *testing.T) {
	d := newSportsDispatcher(t)

	ok, result, errText := invoke(t, d, sportstest.VenuesListCapabilityID, `{}`, "", "")
	if !ok {
		t.Fatalf("venues.list failed: %s", errText)
	}
	var venues struct {
		DataStatus struct {
			State string `json:"state"`
		} `json:"data_status"`
		Venues []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"venues"`
	}
	if err := json.Unmarshal(result, &venues); err != nil {
		t.Fatalf("venues.list result %s: %v", result, err)
	}
	if venues.DataStatus.State != "authoritative_fresh" {
		t.Fatalf("data_status = %+v", venues.DataStatus)
	}
	if len(venues.Venues) != 1 || venues.Venues[0].ID != "venue-1" || venues.Venues[0].Name != "卓尔体育馆" {
		t.Fatalf("venues = %#v", venues.Venues)
	}

	ok, result, errText = invoke(t, d, sportstest.ProjectsListCapabilityID, `{"venue_id":"venue-1"}`, "", "")
	if !ok {
		t.Fatalf("projects.list failed: %s", errText)
	}
	var projects struct {
		Projects []struct {
			ID      string `json:"id"`
			VenueID string `json:"venue_id"`
			Name    string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result, &projects); err != nil {
		t.Fatalf("projects.list result %s: %v", result, err)
	}
	if len(projects.Projects) != 1 || projects.Projects[0].ID != "project-1" || projects.Projects[0].Name != "羽毛球" {
		t.Fatalf("projects = %#v", projects.Projects)
	}

	ok, result, errText = invoke(t, d, sportstest.SlotsSearchCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","date":"`+slotSeedDate(t)+`"}`, "", "")
	if !ok {
		t.Fatalf("slots.search failed: %s", errText)
	}
	var slots struct {
		Slots []struct {
			ID             string `json:"id"`
			RemainingQuota int    `json:"remaining_quota"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(result, &slots); err != nil {
		t.Fatalf("slots.search result %s: %v", result, err)
	}
	if len(slots.Slots) != 1 || slots.Slots[0].ID != "slot-1" || slots.Slots[0].RemainingQuota != 4 {
		t.Fatalf("slots = %#v", slots.Slots)
	}
}

// TestHostedSportsReservationLifecycle 经真实 wasm guest 走通预约生命周期：
// 创建（代名称反规范化）→ 重复预约带内冲突 → mine → 取消（确认门槛）→
// 重复取消带内冲突。
func TestHostedSportsReservationLifecycle(t *testing.T) {
	d := newSportsDispatcher(t)

	ok, result, errText := invoke(t, d, sportstest.ReservationsCreateCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","slot_id":"slot-1","count":2}`, "idem-create", "confirm-create")
	if !ok {
		t.Fatalf("reservations.create failed: %s", errText)
	}
	var created struct {
		Reservation struct {
			ReservationID string `json:"reservation_id"`
			Status        string `json:"status"`
			Count         int    `json:"count"`
			VenueName     string `json:"venue_name"`
			ProjectName   string `json:"project_name"`
		} `json:"reservation"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}
	if created.Reservation.ReservationID == "" || created.Reservation.Status != "confirmed" ||
		created.Reservation.Count != 2 || created.Reservation.VenueName != "卓尔体育馆" ||
		created.Reservation.ProjectName != "羽毛球" {
		t.Fatalf("created = %#v", created.Reservation)
	}
	reservationID := created.Reservation.ReservationID

	// 重复预约同一时段带内冲突。
	ok, result, errText = invoke(t, d, sportstest.ReservationsCreateCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","slot_id":"slot-1"}`, "idem-conflict", "confirm-create")
	if !ok {
		t.Fatalf("double-book should be in-band: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil || conflict.Conflict != "slot_conflict" {
		t.Fatalf("conflict = %#v err=%v result=%s", conflict, err, result)
	}

	// 我的预约列表含已创建项。
	ok, result, errText = invoke(t, d, sportstest.ReservationsMineCapabilityID, `{}`, "", "")
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
	ok, _, errText = invoke(t, d, sportstest.ReservationsCancelCapabilityID,
		`{"reservation_id":"`+reservationID+`"}`, "idem-cancel", "")
	if ok {
		t.Fatal("cancel without confirmation should fail")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("cancel error = %q, want confirmation required", errText)
	}

	ok, result, errText = invoke(t, d, sportstest.ReservationsCancelCapabilityID,
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
	ok, result, errText = invoke(t, d, sportstest.ReservationsCancelCapabilityID,
		`{"reservation_id":"`+reservationID+`"}`, "idem-cancel-2", "confirm-cancel")
	if !ok {
		t.Fatalf("double cancel should be in-band: %s", errText)
	}
	var cancelConflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &cancelConflict); err != nil || cancelConflict.Conflict == "" {
		t.Fatalf("double cancel not marked conflict: %s err=%v", result, err)
	}
}

// TestHostedSportsQuotaExceeded 经真实 wasm guest 验证时段剩余配额：请求人次
// 超出 remaining_quota 按带内 quota_exceeded 应答。
func TestHostedSportsQuotaExceeded(t *testing.T) {
	d := newSportsDispatcher(t)
	ok, result, errText := invoke(t, d, sportstest.ReservationsCreateCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","slot_id":"slot-1","count":16}`, "idem-quota", "confirm-quota")
	if !ok {
		t.Fatalf("over-quota should be in-band: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil || conflict.Conflict != "quota_exceeded" {
		t.Fatalf("conflict = %#v err=%v result=%s", conflict, err, result)
	}
}

// TestHostedSportsOrdersWebview 经真实 wasm guest 获取订单 WebView 描述符：
// 入口 URL 与请求头要求按快照治理返回（不含凭据）。
func TestHostedSportsOrdersWebview(t *testing.T) {
	d := newSportsDispatcher(t)
	ok, result, errText := invoke(t, d, sportstest.OrdersWebviewCapabilityID, `{}`, "", "")
	if !ok {
		t.Fatalf("orders.webview failed: %s", errText)
	}
	var webview struct {
		DataStatus struct {
			State string `json:"state"`
		} `json:"data_status"`
		EntryURL          string `json:"entry_url"`
		RequiredUserAgent string `json:"required_user_agent"`
		RequiredHeaders   []struct {
			Name string `json:"name"`
		} `json:"required_headers"`
	}
	if err := json.Unmarshal(result, &webview); err != nil {
		t.Fatalf("webview result %s: %v", result, err)
	}
	if webview.DataStatus.State != "authoritative_fresh" {
		t.Fatalf("data_status = %+v", webview.DataStatus)
	}
	if webview.EntryURL != "https://orders.example.com/sports" || webview.RequiredUserAgent != "LuoJia/1.0" {
		t.Fatalf("webview = %#v", webview)
	}
	if len(webview.RequiredHeaders) != 1 || webview.RequiredHeaders[0].Name != "X-App-Id" {
		t.Fatalf("headers = %#v", webview.RequiredHeaders)
	}
}

// TestHostedSportsScheduleAddIdempotent 经真实 wasm guest 为预约建立日程：
// 重复添加返回既有日程（幂等）。
func TestHostedSportsScheduleAddIdempotent(t *testing.T) {
	d := newSportsDispatcher(t)
	ok, result, errText := invoke(t, d, sportstest.ReservationsCreateCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","slot_id":"slot-1"}`, "idem-create", "confirm-create")
	if !ok {
		t.Fatalf("create failed: %s", errText)
	}
	var created struct {
		Reservation struct {
			ReservationID string `json:"reservation_id"`
		} `json:"reservation"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}

	// 日程添加是确认门槛能力。
	ok, _, errText = invoke(t, d, sportstest.ScheduleAddCapabilityID,
		`{"reservation_id":"`+created.Reservation.ReservationID+`"}`, "idem-sched", "")
	if ok {
		t.Fatal("schedule.add without confirmation should fail")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("schedule.add error = %q, want confirmation required", errText)
	}

	ok, result, errText = invoke(t, d, sportstest.ScheduleAddCapabilityID,
		`{"reservation_id":"`+created.Reservation.ReservationID+`"}`, "idem-sched", "confirm-sched")
	if !ok {
		t.Fatalf("schedule.add failed: %s", errText)
	}
	var first struct {
		Schedule struct {
			ScheduleID    string `json:"schedule_id"`
			Title         string `json:"title"`
			ReservationID string `json:"reservation_id"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(result, &first); err != nil {
		t.Fatalf("schedule result %s: %v", result, err)
	}
	if first.Schedule.Title != "卓尔体育馆 羽毛球" || first.Schedule.ReservationID != created.Reservation.ReservationID {
		t.Fatalf("schedule = %#v", first.Schedule)
	}

	ok, result, errText = invoke(t, d, sportstest.ScheduleAddCapabilityID,
		`{"reservation_id":"`+created.Reservation.ReservationID+`"}`, "idem-sched-2", "confirm-sched")
	if !ok {
		t.Fatalf("schedule.add repeat failed: %s", errText)
	}
	var second struct {
		Schedule struct {
			ScheduleID string `json:"schedule_id"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(result, &second); err != nil || second.Schedule.ScheduleID != first.Schedule.ScheduleID {
		t.Fatalf("schedule.add not idempotent: %s vs %#v err=%v", result, first.Schedule.ScheduleID, err)
	}
}

// TestHostedSportsCreatePastSlotFailClosed 已结束时段创建按 data_expired 稳定
// 错误码拒绝（fail-closed；错误码只进 InvocationError，消息不外泄）。
func TestHostedSportsCreatePastSlotFailClosed(t *testing.T) {
	reg := registry.New()
	store := hostedtest.MemoryStore()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: sportstest.PackageID, PackageID: sportstest.PackageID, Namespace: sportstest.StorageNamespace}
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"venues":   {hostedtest.MustDoc(t, "venue-1", map[string]any{"id": "venue-1", "name": "卓尔体育馆", "source_revision": "rev-1"})},
		"projects": {hostedtest.MustDoc(t, "project-1", map[string]any{"id": "project-1", "venue_id": "venue-1", "name": "羽毛球", "source_revision": "rev-1"})},
		"slots": {hostedtest.MustDoc(t, "slot-1", map[string]any{
			"id": "slot-1", "venue_id": "venue-1", "project_id": "project-1", "date": "2020-01-01",
			"start_at": "2020-01-01T00:00:00Z", "end_at": "2020-01-01T02:00:00Z",
			"capacity": 4, "remaining_quota": 4, "source_revision": "rev-1",
		})},
	}); err != nil {
		t.Fatal(err)
	}
	sportstest.RegisterHosted(t, reg, store)
	d := hostedtest.NewDispatcher(t, reg, sportstest.PackageID, sportstest.CapabilityIDs())
	_, _, errText := invoke(t, d, sportstest.ReservationsCreateCapabilityID,
		`{"venue_id":"venue-1","project_id":"project-1","slot_id":"slot-1"}`, "idem-past", "confirm-past")
	if errText == "" {
		t.Fatal("past slot create should fail")
	}
	if !strings.Contains(errText, "hosted package rejected the call") {
		t.Fatalf("error = %q, want hosted rejection", errText)
	}
}

// TestHostedSportsGovernedSnapshotRejection 非权威快照经 guest 侧治理以稳定
// 错误码拒绝：guest 信封 data_untrusted 在内核错误面投影为
// data_non_authoritative（细节不外泄）。
func TestHostedSportsGovernedSnapshotRejection(t *testing.T) {
	reg := registry.New()
	store := hostedtest.MemoryStore()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: sportstest.PackageID, PackageID: sportstest.PackageID, Namespace: sportstest.StorageNamespace}
	demo := hostedtest.AuthoritativeMeta(now)
	demo.Authoritative = false
	demo.Source = "demo-fixture"
	if err := store.ReplaceSnapshot(context.Background(), scope, demo, map[string][]packstore.Document{
		"venues": {hostedtest.MustDoc(t, "venue-1", map[string]any{"id": "venue-1", "name": "卓尔体育馆", "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
	sportstest.RegisterHosted(t, reg, store)
	d := hostedtest.NewDispatcher(t, reg, sportstest.PackageID, sportstest.CapabilityIDs())
	request := hostedtest.RequestContext(sportstest.PackageID, "request-governed-rejection")
	_, invokeErr := d.InvokeCapability(t.Context(), request, sportstest.VenuesListCapabilityID, json.RawMessage(`{}`))
	if invokeErr == nil || !strings.Contains(invokeErr.Error(), "hosted package rejected the call") {
		t.Fatalf("err = %v", invokeErr)
	}
	var invocation loader.InvocationError
	if !errors.As(invokeErr, &invocation) || invocation.Code != "data_non_authoritative" {
		t.Fatalf("invocation error = %#v, want data_non_authoritative", invocation)
	}
}

// slotSeedDate 读取播种时段的学术日期（与 seedSports 的 slotDate 同源推导）。
func slotSeedDate(t *testing.T) string {
	t.Helper()
	slotStart := time.Now().UTC().AddDate(0, 0, 1).Truncate(time.Hour)
	return slotStart.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
}
