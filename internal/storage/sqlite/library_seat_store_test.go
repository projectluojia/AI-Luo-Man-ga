package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	libraryseat "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

func TestLibrarySeatStoreIsolationRaceAndStateMachine(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "library-seat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	if _, err := store.CreateUser(ctx, identity.User{UserID: "user-a", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(ctx, identity.User{UserID: "user-b", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	catalog := testCatalog(now, "campus-services")
	if err := store.ReplaceCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	other := testCatalog(now, "app-b")
	if err := store.ReplaceCatalog(ctx, other); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListSpaces(ctx, "campus-services", libraryseat.SpaceListRequest{Limit: 50})
	if err != nil || len(listed.Spaces) != 1 {
		t.Fatalf("list=%#v err=%v", listed, err)
	}
	isolated, err := store.ListSpaces(ctx, "app-b", libraryseat.SpaceListRequest{Limit: 50})
	if err != nil || isolated.Metadata.Revision != "rev-app-b" {
		t.Fatalf("isolation=%#v err=%v", isolated, err)
	}
	missing, err := store.ListSpaces(ctx, "missing-app", libraryseat.SpaceListRequest{Limit: 50})
	if !errors.Is(err, contracts.ErrDataUnavailable) || len(missing.Spaces) != 0 {
		t.Fatalf("missing snapshot=%#v err=%v", missing, err)
	}

	first, err := store.CreateReservation(ctx, libraryseat.CreateReservationInput{
		AppID: "campus-services", UserID: "user-a", SpaceID: "space-a", SeatID: "seat-1",
		SlotID: "slot-morning", Date: "2026-09-02", IdempotencyKey: "key-a",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.CreateReservation(ctx, libraryseat.CreateReservationInput{
		AppID: "campus-services", UserID: "user-a", SpaceID: "space-a", SeatID: "seat-1",
		SlotID: "slot-morning", Date: "2026-09-02", IdempotencyKey: "key-a",
	}, now)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err := store.CreateReservation(ctx, libraryseat.CreateReservationInput{
		AppID: "campus-services", UserID: "user-a", SpaceID: "space-a", SeatID: "seat-2",
		SlotID: "slot-morning", Date: "2026-09-02", IdempotencyKey: "key-a",
	}, now); !errors.Is(err, libraryseat.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		user := "user-a"
		if i == 1 {
			user = "user-b"
		}
		go func(userID, key string) {
			defer wg.Done()
			_, err := store.CreateReservation(ctx, libraryseat.CreateReservationInput{
				AppID: "campus-services", UserID: userID, SpaceID: "space-a", SeatID: "seat-2",
				SlotID: "slot-morning", Date: "2026-09-02", IdempotencyKey: key,
			}, now)
			errs <- err
		}(user, "race-"+user)
	}
	wg.Wait()
	close(errs)
	var successes, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, libraryseat.ErrConflict), errors.Is(err, libraryseat.ErrQuotaExceeded):
			conflicts++
		default:
			t.Fatalf("race err=%v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("race successes=%d conflicts=%d", successes, conflicts)
	}

	if _, err := store.CancelReservation(ctx, libraryseat.CancelReservationInput{
		AppID: "campus-services", UserID: "user-b", ReservationID: first.ID,
	}, now); !errors.Is(err, libraryseat.ErrNotOwner) {
		t.Fatalf("cross-user cancel err=%v", err)
	}
	cancelled, err := store.CancelReservation(ctx, libraryseat.CancelReservationInput{
		AppID: "campus-services", UserID: "user-a", ReservationID: first.ID,
	}, now)
	if err != nil || cancelled.Status != libraryseat.ReservationCancelled {
		t.Fatalf("cancel=%#v err=%v", cancelled, err)
	}
	again, err := store.CancelReservation(ctx, libraryseat.CancelReservationInput{
		AppID: "campus-services", UserID: "user-a", ReservationID: first.ID,
	}, now)
	if err != nil || again.Status != libraryseat.ReservationCancelled {
		t.Fatalf("idempotent cancel=%#v err=%v", again, err)
	}
	if _, err := store.CancelReservation(ctx, libraryseat.CancelReservationInput{
		AppID: "campus-services", UserID: "user-a", ReservationID: first.ID,
	}, now.Add(48*time.Hour)); err != nil && !errors.Is(err, libraryseat.ErrIllegalTransition) && !errors.Is(err, libraryseat.ErrNotFound) {
		t.Fatalf("late cancel err=%v", err)
	}
}

func testCatalog(now time.Time, appID string) libraryseat.CatalogSnapshot {
	revision := "rev-" + appID
	return libraryseat.CatalogSnapshot{
		AppID: appID, Revision: revision, Source: libraryseat.DemoSource,
		Authoritative: false, Complete: true, ImportedAt: now, ValidUntil: now.Add(72 * time.Hour),
		Spaces: []libraryseat.Space{{ID: "space-a", Name: "演示阅览室", SourceRevision: revision}},
		Seats: []libraryseat.Seat{
			{ID: "seat-1", SpaceID: "space-a", Label: "A01", SourceRevision: revision},
			{ID: "seat-2", SpaceID: "space-a", Label: "A02", SourceRevision: revision},
		},
		Slots: []libraryseat.Slot{{ID: "slot-morning", Name: "上午", StartMinute: 8 * 60, EndMinute: 12 * 60, SourceRevision: revision}},
	}
}
