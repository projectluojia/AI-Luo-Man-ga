package sports_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

func TestSportsToolsQueryAndFailClosed(t *testing.T) {
	store, now, handlers := sportsToolHarness(t)
	ctx := context.Background()
	request := sportsRequest("user-1")

	seedAuthoritativeSports(t, store, now, 4)
	raw, err := handlers[sports.VenueListToolID](ctx, request, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var venues sports.VenueListResult
	if err := json.Unmarshal(raw, &venues); err != nil || len(venues.Venues) != 1 || venues.DataStatus.State != sports.DataStateAuthoritativeFresh {
		t.Fatalf("venues=%s err=%v", raw, err)
	}

	raw, err = handlers[sports.ProjectListToolID](ctx, request, json.RawMessage(`{"venue_id":"venue-gym"}`))
	if err != nil {
		t.Fatal(err)
	}
	var projects sports.ProjectListResult
	if err := json.Unmarshal(raw, &projects); err != nil || len(projects.Projects) != 1 {
		t.Fatalf("projects=%s err=%v", raw, err)
	}

	raw, err = handlers[sports.SlotSearchToolID](ctx, request, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","date":"2026-09-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	var slots sports.SlotSearchResult
	if err := json.Unmarshal(raw, &slots); err != nil || len(slots.Slots) != 1 || slots.Slots[0].RemainingQuota != 4 {
		t.Fatalf("slots=%s err=%v", raw, err)
	}

	seedAuthoritativeSportsMeta(t, store, now, 4, false)
	_, err = handlers[sports.VenueListToolID](ctx, request, json.RawMessage(`{}`))
	if !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("demo catalog err=%v", err)
	}
}

func TestSportsReservationOverQuotaAndIdempotentCancel(t *testing.T) {
	store, now, handlers := sportsToolHarness(t)
	seedAuthoritativeSports(t, store, now, 2)
	ctx := context.Background()
	request := sportsRequest("user-1")

	_, err := handlers[sports.ReservationCreateToolID](ctx, request, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":3}`))
	if !errors.Is(err, sports.ErrOverQuota) {
		t.Fatalf("over-quota err=%v", err)
	}

	raw, err := handlers[sports.ReservationCreateToolID](ctx, request, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var created sports.ReservationResult
	if err := json.Unmarshal(raw, &created); err != nil || created.Reservation.Status != sports.StatusConfirmed {
		t.Fatalf("created=%s err=%v", raw, err)
	}

	payload, _ := json.Marshal(map[string]string{"reservation_id": created.Reservation.ID})
	if _, err = handlers[sports.ReservationCancelToolID](ctx, request, payload); err != nil {
		t.Fatal(err)
	}
	raw, err = handlers[sports.ReservationCancelToolID](ctx, request, payload)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled sports.ReservationResult
	if err := json.Unmarshal(raw, &cancelled); err != nil || cancelled.Reservation.Status != sports.StatusCancelled {
		t.Fatalf("cancelled=%s err=%v", raw, err)
	}
}

func TestSportsWebViewDescriptorHasNoSecrets(t *testing.T) {
	store, now, handlers := sportsToolHarness(t)
	if err := demo.LoadSportsData(context.Background(), store, now); err != nil {
		t.Fatal(err)
	}
	raw, err := handlers[sports.OrdersWebViewToolID](context.Background(), sportsRequest("user-1"), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if sports.ContainsSensitiveWebViewKey(raw) {
		t.Fatalf("webview leaked secrets: %s", raw)
	}
	var result sports.WebViewResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.DataStatus.State != sports.DataStateDemoNonAuthoritative || result.DataStatus.Authoritative ||
		result.EntryURL != "https://demo.ailuo.invalid/sports/orders" || result.RequiresDelegatedAuth {
		t.Fatalf("webview=%#v", result)
	}
	for _, header := range result.RequiredHeaders {
		if header.Name == "" || header.Purpose == "" {
			t.Fatalf("header missing name/purpose: %#v", header)
		}
	}

	seedAuthoritativeSports(t, store, now, 2)
	_, err = handlers[sports.OrdersWebViewToolID](context.Background(), sportsRequest("user-1"), json.RawMessage(`{}`))
	if !errors.Is(err, sports.ErrDelegatedAuthRequired) {
		t.Fatalf("authoritative webview err=%v", err)
	}
}

func TestSportsToolSpecsRequireConfirmationOnWrites(t *testing.T) {
	wanted := map[string]bool{
		sports.ReservationCreateToolID: true,
		sports.ReservationCancelToolID: true,
		sports.ScheduleAddToolID:       true,
		sports.VenueListToolID:         false,
		sports.OrdersWebViewToolID:     false,
	}
	for _, spec := range sports.ToolSpecs() {
		expect, ok := wanted[spec.ID]
		if !ok {
			continue
		}
		if spec.RequiresConfirmation != expect {
			t.Fatalf("tool %s RequiresConfirmation=%v want %v", spec.ID, spec.RequiresConfirmation, expect)
		}
		if expect && spec.SideEffect != registry.SideEffectWrite {
			t.Fatalf("tool %s side effect=%s", spec.ID, spec.SideEffect)
		}
	}
}

func sportsToolHarness(t *testing.T) (*sqlite.Store, time.Time, map[string]registry.Handler) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "sports.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, dir) })
	if _, err := identity.NewService(store).CreateUser(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 8, 0, 0, 0, location)
	return store, now, sports.ToolHandlers(store, func() time.Time { return now })
}

func sportsRequest(userID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: campus.AppID, EchoID: "echo-sports", RequestID: "req-sports", UserID: userID,
		Deadline: time.Now().Add(time.Minute),
	}
}

func seedAuthoritativeSports(t *testing.T, store *sqlite.Store, now time.Time, capacity int) {
	t.Helper()
	seedAuthoritativeSportsMeta(t, store, now, capacity, true)
}

func seedAuthoritativeSportsMeta(t *testing.T, store *sqlite.Store, now time.Time, capacity int, authoritative bool) {
	t.Helper()
	start := time.Date(2026, time.September, 1, 10, 0, 0, 0, now.Location())
	if err := store.ReplaceSportsSnapshot(context.Background(), sports.CatalogSnapshot{
		AppID: campus.AppID, Revision: "rev-sports-1", Source: "test-fixture",
		Authoritative: authoritative, Complete: true,
		ImportedAt: now.Add(-time.Minute), ValidUntil: now.Add(24 * time.Hour),
		Venues:   []sports.Venue{{ID: "venue-gym", Name: "演示体育馆", SourceRevision: "rev-sports-1"}},
		Projects: []sports.Project{{ID: "project-badminton", VenueID: "venue-gym", Name: "羽毛球", SourceRevision: "rev-sports-1"}},
		Slots: []sports.Slot{{
			ID: "slot-morning", VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-01",
			StartAt: start, EndAt: start.Add(90 * time.Minute), Capacity: capacity, RemainingQuota: capacity, SourceRevision: "rev-sports-1",
		}},
		WebView: sports.WebViewDescriptor{
			EntryURL: "https://demo.ailuo.invalid/sports/orders", RequiredUserAgent: "AiluoCampusClient/1.0 (test)",
			RequiredHeaders: []sports.RequiredHeader{{Name: "Referer", Purpose: "Satisfy same-site navigation checks without carrying secrets"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}
