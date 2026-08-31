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
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	sportsservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/sports"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

type allowConfirmations struct{}

func (allowConfirmations) VerifyConfirmation(context.Context, runtime.ConfirmationRequest) error {
	return nil
}

func TestSportsRegisterExposesGovernedCapabilities(t *testing.T) {
	store, dispatcher, now := sportsDispatcher(t)
	seedSports(t, store, now, 2)

	request := sportsCapabilityRequest("user-1", "idem-list")
	raw, err := dispatcher.InvokeCapability(context.Background(), request, sportsservice.VenuesListCapabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var venues sports.VenueListResult
	if err := json.Unmarshal(raw, &venues); err != nil || len(venues.Venues) != 1 {
		t.Fatalf("venues=%s err=%v", raw, err)
	}

	_, err = dispatcher.InvokeCapability(context.Background(), request, sportsservice.ReservationsCreateCapabilityID, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":1}`))
	if !errors.Is(err, runtime.ErrConfirmationRequired) {
		t.Fatalf("create without confirmation err=%v", err)
	}

	createReq := sportsCapabilityRequest("user-1", "idem-create")
	createReq.ConfirmationID = "confirmation-create"
	raw, err = dispatcher.InvokeCapability(context.Background(), createReq, sportsservice.ReservationsCreateCapabilityID, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":1}`))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := dispatcher.InvokeCapability(context.Background(), createReq, sportsservice.ReservationsCreateCapabilityID, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(replay) {
		t.Fatalf("idempotent create drifted: %s vs %s", raw, replay)
	}

	overReq := sportsCapabilityRequest("user-2", "idem-over")
	overReq.ConfirmationID = "confirmation-over"
	_, err = dispatcher.InvokeCapability(context.Background(), overReq, sportsservice.ReservationsCreateCapabilityID, json.RawMessage(`{"venue_id":"venue-gym","project_id":"project-badminton","slot_id":"slot-morning","count":2}`))
	if !errors.Is(err, sports.ErrOverQuota) {
		t.Fatalf("over-quota via dispatcher err=%v", err)
	}
	public := publicerror.Capability(err)
	if public.Code != "quota_exceeded" {
		t.Fatalf("public over-quota=%#v", public)
	}
}

func TestSportsCapabilitySpecsRequireConfirmation(t *testing.T) {
	reg := registry.New()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "sports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	if err := sportsservice.Register(reg, sportsservice.NewService(store)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		sportsservice.ReservationsCreateCapabilityID,
		sportsservice.ReservationsCancelCapabilityID,
		sportsservice.ScheduleAddCapabilityID,
	} {
		spec, _, err := reg.ResolveCapability(id)
		if err != nil || !spec.RequiresConfirmation || spec.SideEffect != registry.SideEffectWrite {
			t.Fatalf("capability %s spec=%#v err=%v", id, spec, err)
		}
	}
	for _, id := range []string{
		sportsservice.VenuesListCapabilityID,
		sportsservice.OrdersWebViewCapabilityID,
	} {
		spec, _, err := reg.ResolveCapability(id)
		if err != nil || spec.RequiresConfirmation {
			t.Fatalf("read capability %s spec=%#v err=%v", id, spec, err)
		}
	}
}

func sportsDispatcher(t *testing.T) (*sqlite.Store, *runtime.Dispatcher, time.Time) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "sports.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, dir) })
	identities := identity.NewService(store)
	if _, err := identities.CreateUser(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := identities.CreateUser(context.Background(), "user-2"); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 8, 0, 0, 0, location)
	if err := sportsservice.Register(reg, sportsservice.NewServiceWithNow(store, func() time.Time { return now })); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	for _, id := range sportsservice.CapabilityIDs() {
		policy.Enable(campus.AppID, id)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: allowConfirmations{},
	})
	return store, dispatcher, now
}

func sportsCapabilityRequest(userID, idempotencyKey string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: campus.AppID, EchoID: "echo-sports", RequestID: "req-" + idempotencyKey,
		UserID: userID, RunID: "run-sports", Deadline: time.Now().Add(time.Minute),
		IdempotencyKey: idempotencyKey,
	}
}

func seedSports(t *testing.T, store *sqlite.Store, now time.Time, capacity int) {
	t.Helper()
	start := time.Date(2026, time.September, 1, 10, 0, 0, 0, now.Location())
	if err := store.ReplaceSportsSnapshot(context.Background(), sports.CatalogSnapshot{
		AppID: campus.AppID, Revision: "rev-sports-1", Source: "test-fixture",
		Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Minute), ValidUntil: now.Add(24 * time.Hour),
		Venues:   []sports.Venue{{ID: "venue-gym", Name: "演示体育馆", SourceRevision: "rev-sports-1"}},
		Projects: []sports.Project{{ID: "project-badminton", VenueID: "venue-gym", Name: "羽毛球", SourceRevision: "rev-sports-1"}},
		Slots: []sports.Slot{{
			ID: "slot-morning", VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-01",
			StartAt: start, EndAt: start.Add(90 * time.Minute), Capacity: capacity, RemainingQuota: capacity, SourceRevision: "rev-sports-1",
		}},
		WebView: sports.WebViewDescriptor{
			EntryURL: "https://demo.ailuo.invalid/sports/orders", RequiredUserAgent: "AiluoCampusClient/1.0 (test)",
			RequiredHeaders: []sports.RequiredHeader{{Name: "X-Requested-With", Purpose: "Identify the governed campus client runtime"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}
