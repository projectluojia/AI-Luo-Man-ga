package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

func TestSportsMigration27CreatesIsolatedTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sports.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 27 {
		t.Fatalf("schema version=%d, want >= 27", version)
	}
	for _, table := range []string{
		"sports_source_revisions", "sports_current_snapshots", "sports_venues", "sports_projects",
		"sports_slots", "sports_webview_descriptors", "sports_reservations", "sports_schedule_items",
	} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil || name != table {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
}

func TestSportsStoreAppIsolationAndQueries(t *testing.T) {
	store, now := openSportsStore(t)
	ctx := context.Background()
	seedSportsCatalog(t, store, campus.AppID, true, 4, now)
	seedSportsCatalog(t, store, "other-app", true, 4, now)

	venues, err := store.ListVenues(ctx, campus.AppID)
	if err != nil || len(venues.Venues) != 1 || venues.Venues[0].ID != "venue-gym" {
		t.Fatalf("venues=%#v err=%v", venues, err)
	}
	other, err := store.ListVenues(ctx, "missing-app")
	if !errors.Is(err, contracts.ErrDataUnavailable) || len(other.Venues) != 0 {
		t.Fatalf("cross-app venues=%#v err=%v", other, err)
	}
	projects, err := store.ListProjects(ctx, campus.AppID, sports.ProjectListRequest{VenueID: "venue-gym"})
	if err != nil || len(projects.Projects) != 1 || projects.Projects[0].ID != "project-badminton" {
		t.Fatalf("projects=%#v err=%v", projects, err)
	}
	slots, err := store.SearchSlots(ctx, campus.AppID, sports.SlotSearchRequest{
		VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-01",
	})
	if err != nil || len(slots.Slots) != 1 || slots.Slots[0].RemainingQuota != 4 {
		t.Fatalf("slots=%#v err=%v", slots, err)
	}
}

func TestSportsStoreCreateCancelQuotaAndSchedule(t *testing.T) {
	store, now := openSportsStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	seedSportsCatalog(t, store, campus.AppID, true, 2, now)

	created, _, err := store.CreateReservation(ctx, sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 2, Now: now,
	})
	if err != nil || created.Status != sports.StatusConfirmed {
		t.Fatalf("create=%#v err=%v", created, err)
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 0 {
		t.Fatalf("remaining after exact quota=%d", remaining)
	}

	mine, err := store.ListMyReservations(ctx, campus.AppID, "user-1", now)
	if err != nil || len(mine.Reservations) != 1 || mine.Reservations[0].ID != created.ID {
		t.Fatalf("mine=%#v err=%v", mine, err)
	}

	schedule, _, err := store.AddScheduleItem(ctx, sports.AddScheduleInput{
		AppID: campus.AppID, UserID: "user-1", ReservationID: created.ID, Now: now,
	})
	if err != nil || schedule.ReservationID != created.ID || schedule.Title == "" {
		t.Fatalf("schedule=%#v err=%v", schedule, err)
	}
	replay, _, err := store.AddScheduleItem(ctx, sports.AddScheduleInput{
		AppID: campus.AppID, UserID: "user-1", ReservationID: created.ID, Now: now,
	})
	if err != nil || replay.ID != schedule.ID {
		t.Fatalf("schedule replay=%#v err=%v", replay, err)
	}

	cancelled, _, err := store.CancelReservation(ctx, sports.CancelReservationInput{
		AppID: campus.AppID, UserID: "user-1", ReservationID: created.ID, Now: now,
	})
	if err != nil || cancelled.Status != sports.StatusCancelled {
		t.Fatalf("cancel=%#v err=%v", cancelled, err)
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 2 {
		t.Fatalf("remaining after cancel=%d", remaining)
	}
	again, _, err := store.CancelReservation(ctx, sports.CancelReservationInput{
		AppID: campus.AppID, UserID: "user-1", ReservationID: created.ID, Now: now,
	})
	if err != nil || again.Status != sports.StatusCancelled {
		t.Fatalf("cancel replay=%#v err=%v", again, err)
	}
}

func TestSportsStoreRejectsOverQuotaWithoutPersist(t *testing.T) {
	store, now := openSportsStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	seedSportsCatalog(t, store, campus.AppID, true, 2, now)

	_, _, err := store.CreateReservation(ctx, sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 3, Now: now,
	})
	if !errors.Is(err, sports.ErrOverQuota) {
		t.Fatalf("over-quota err=%v", err)
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 2 {
		t.Fatalf("quota mutated after over-quota remaining=%d", remaining)
	}
	mine, err := store.ListMyReservations(ctx, campus.AppID, "user-1", now)
	if err != nil || len(mine.Reservations) != 0 {
		t.Fatalf("over-quota persisted reservation=%#v err=%v", mine, err)
	}
}

func TestSportsStoreConcurrentOverbook(t *testing.T) {
	store, now := openSportsStore(t)
	ctx := context.Background()
	for i := 1; i <= 8; i++ {
		mustCreateUser(t, store, "user-"+itoa(i))
	}
	seedSportsCatalog(t, store, campus.AppID, true, 1, now)

	var success, overQuota atomic.Int32
	var group sync.WaitGroup
	group.Add(8)
	for i := 1; i <= 8; i++ {
		go func(userID string) {
			defer group.Done()
			_, _, err := store.CreateReservation(ctx, sports.CreateReservationInput{
				AppID: campus.AppID, UserID: userID, VenueID: "venue-gym", ProjectID: "project-badminton",
				SlotID: "slot-morning", Count: 1, Now: now,
			})
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, sports.ErrOverQuota):
				overQuota.Add(1)
			default:
				t.Errorf("unexpected concurrent error: %v", err)
			}
		}("user-" + itoa(i))
	}
	group.Wait()
	if success.Load() != 1 || overQuota.Load() != 7 {
		t.Fatalf("success=%d over_quota=%d", success.Load(), overQuota.Load())
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 0 {
		t.Fatalf("remaining after concurrent overbook=%d", remaining)
	}
}

func TestSportsStoreCancelAndTimeout(t *testing.T) {
	store, now := openSportsStore(t)
	mustCreateUser(t, store, "user-1")
	seedSportsCatalog(t, store, campus.AppID, true, 2, now)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := store.CreateReservation(cancelled, sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 1, Now: now,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context err=%v", err)
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 2 {
		t.Fatalf("quota mutated after cancel remaining=%d", remaining)
	}

	created, _, err := store.CreateReservation(context.Background(), sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 1, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(4 * time.Hour)
	mine, err := store.ListMyReservations(context.Background(), campus.AppID, "user-1", expiredAt)
	if err != nil || len(mine.Reservations) != 1 || mine.Reservations[0].Status != sports.StatusExpired {
		t.Fatalf("expired listing=%#v err=%v", mine, err)
	}
	_, _, err = store.CancelReservation(context.Background(), sports.CancelReservationInput{
		AppID: campus.AppID, UserID: "user-1", ReservationID: created.ID, Now: expiredAt,
	})
	if !errors.Is(err, sports.ErrNotCancellable) {
		t.Fatalf("cancel expired err=%v", err)
	}
}

func TestSportsReservationTimesSurviveSnapshotReplace(t *testing.T) {
	store, now := openSportsStore(t)
	mustCreateUser(t, store, "user-1")
	seedSportsCatalog(t, store, campus.AppID, true, 2, now)
	created, _, err := store.CreateReservation(context.Background(), sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 1, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement := now.Add(time.Minute)
	start := time.Date(2026, time.September, 2, 10, 0, 0, 0, now.Location())
	if err := store.ReplaceSportsSnapshot(context.Background(), sports.CatalogSnapshot{
		AppID: campus.AppID, Revision: "rev-sports-2", Source: "test-fixture",
		Authoritative: true, Complete: true,
		ImportedAt: replacement, ValidUntil: replacement.Add(24 * time.Hour),
		Venues:   []sports.Venue{{ID: "venue-gym", Name: "演示体育馆", SourceRevision: "rev-sports-2"}},
		Projects: []sports.Project{{ID: "project-badminton", VenueID: "venue-gym", Name: "羽毛球", SourceRevision: "rev-sports-2"}},
		Slots: []sports.Slot{{
			ID: "slot-other", VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-02",
			StartAt: start, EndAt: start.Add(90 * time.Minute), Capacity: 2, RemainingQuota: 2, SourceRevision: "rev-sports-2",
		}},
		WebView: sports.WebViewDescriptor{
			EntryURL: "https://demo.ailuo.invalid/sports/orders", RequiredUserAgent: "AiluoCampusClient/1.0 (test)",
			RequiredHeaders: []sports.RequiredHeader{{Name: "X-Requested-With", Purpose: "Identify the governed campus client runtime"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	mine, err := store.ListMyReservations(context.Background(), campus.AppID, "user-1", now)
	if err != nil || len(mine.Reservations) != 1 || mine.Reservations[0].ID != created.ID {
		t.Fatalf("mine after snapshot replace=%#v err=%v", mine, err)
	}
	if mine.Reservations[0].StartAt.IsZero() || mine.Reservations[0].EndAt.IsZero() || mine.Reservations[0].Status != sports.StatusConfirmed {
		t.Fatalf("lost reservation times after snapshot replace: %#v", mine.Reservations[0])
	}
	expired, err := store.ListMyReservations(context.Background(), campus.AppID, "user-1", now.Add(4*time.Hour))
	if err != nil || len(expired.Reservations) != 1 || expired.Reservations[0].Status != sports.StatusExpired {
		t.Fatalf("expire without slot row=%#v err=%v", expired, err)
	}
}

func TestSportsCreateRejectsStaleRevisionBeforeWrite(t *testing.T) {
	store, now := openSportsStore(t)
	mustCreateUser(t, store, "user-1")
	seedSportsCatalog(t, store, campus.AppID, true, 2, now)
	_, _, err := store.CreateReservation(context.Background(), sports.CreateReservationInput{
		AppID: campus.AppID, UserID: "user-1", VenueID: "venue-gym", ProjectID: "project-badminton",
		SlotID: "slot-morning", Count: 1, Now: now, ExpectedRevision: "rev-stale",
	})
	if !errors.Is(err, contracts.ErrDataIncomplete) {
		t.Fatalf("stale revision err=%v", err)
	}
	mine, err := store.ListMyReservations(context.Background(), campus.AppID, "user-1", now)
	if err != nil || len(mine.Reservations) != 0 {
		t.Fatalf("stale revision persisted reservation=%#v err=%v", mine, err)
	}
	if remaining := remainingSportsQuota(t, store, now); remaining != 2 {
		t.Fatalf("quota mutated after stale revision remaining=%d", remaining)
	}
}

func TestSportsDemoSnapshotIsNonAuthoritative(t *testing.T) {
	store, _ := openSportsStore(t)
	if err := demo.LoadSportsData(context.Background(), store, time.Now()); err != nil {
		t.Fatal(err)
	}
	venues, err := store.ListVenues(context.Background(), campus.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if venues.Metadata.Authoritative || venues.Metadata.Source != "demo-fixture-not-zhihui-luojia" {
		t.Fatalf("demo metadata=%#v", venues.Metadata)
	}
	webview, err := store.GetWebViewDescriptor(context.Background(), campus.AppID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(webview.Descriptor)
	if sports.ContainsSensitiveWebViewKey(raw) {
		t.Fatalf("demo webview leaked secrets: %s", raw)
	}
}

func openSportsStore(t *testing.T) (*sqlite.Store, time.Time) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "sports.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, dir) })
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 8, 0, 0, 0, location)
	return store, now
}

func seedSportsCatalog(t *testing.T, store *sqlite.Store, appID string, authoritative bool, capacity int, now time.Time) {
	t.Helper()
	location := now.Location()
	start := time.Date(2026, time.September, 1, 10, 0, 0, 0, location)
	snapshot := sports.CatalogSnapshot{
		AppID: appID, Revision: "rev-sports-1", Source: "test-fixture",
		Authoritative: authoritative, Complete: true,
		ImportedAt: now.Add(-time.Minute), ValidUntil: now.Add(24 * time.Hour),
		Venues:   []sports.Venue{{ID: "venue-gym", Name: "演示体育馆", Campus: "信息学部", SourceRevision: "rev-sports-1"}},
		Projects: []sports.Project{{ID: "project-badminton", VenueID: "venue-gym", Name: "羽毛球", SourceRevision: "rev-sports-1"}},
		Slots: []sports.Slot{{
			ID: "slot-morning", VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-01",
			StartAt: start, EndAt: start.Add(90 * time.Minute), Capacity: capacity, RemainingQuota: capacity, SourceRevision: "rev-sports-1",
		}},
		WebView: sports.WebViewDescriptor{
			EntryURL: "https://demo.ailuo.invalid/sports/orders", RequiredUserAgent: "AiluoCampusClient/1.0 (test)",
			RequiredHeaders:       []sports.RequiredHeader{{Name: "X-Requested-With", Purpose: "Identify the governed campus client runtime"}},
			RequiresDelegatedAuth: !authoritative,
			SourceRevision:        "rev-sports-1",
		},
	}
	if err := store.ReplaceSportsSnapshot(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
}

func remainingSportsQuota(t *testing.T, store *sqlite.Store, now time.Time) int {
	t.Helper()
	_ = now
	slots, err := store.SearchSlots(t.Context(), campus.AppID, sports.SlotSearchRequest{
		VenueID: "venue-gym", ProjectID: "project-badminton", Date: "2026-09-01",
	})
	if err != nil || len(slots.Slots) != 1 {
		t.Fatalf("slots=%#v err=%v", slots, err)
	}
	return slots.Slots[0].RemainingQuota
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
