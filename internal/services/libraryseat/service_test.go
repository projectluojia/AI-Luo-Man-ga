package libraryseat

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	libraryseattool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

type acceptConfirmation struct{}

func (acceptConfirmation) VerifyConfirmation(context.Context, runtime.ConfirmationRequest) error {
	return nil
}

func TestLibrarySeatCapabilities(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "library-seat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	mustCreateUser(t, store, "user-2")
	clock := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC) // 上海 10:00
	reg := registry.New()
	if err := NewServiceWithClock(store, func() time.Time { return clock }).Register(reg); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	for _, id := range CapabilityIDs() {
		policy.Enable("campus-services", id)
		policy.Enable("other-app", id)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: acceptConfirmation{},
	})

	t.Run("missing snapshot is fail-closed not empty list", func(t *testing.T) {
		_, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SpacesListCapabilityID, []byte(`{}`))
		if !errors.Is(err, contracts.ErrDataUnavailable) {
			t.Fatalf("missing snapshot err=%v", err)
		}
	})

	catalog := demoCatalog(clock, "campus-services")
	if err := store.ReplaceCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	other := demoCatalog(clock, "other-app")
	if err := store.ReplaceCatalog(ctx, other); err != nil {
		t.Fatal(err)
	}

	t.Run("list spaces and empty filter", func(t *testing.T) {
		raw, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SpacesListCapabilityID, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var listed libraryseattool.SpaceListResult
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatal(err)
		}
		if listed.DataStatus.State != libraryseattool.DataStateDemoNonAuthoritative || listed.DataStatus.Authoritative ||
			listed.DataStatus.Source != libraryseattool.DemoSource || len(listed.Spaces) != 2 {
			t.Fatalf("listed=%#v", listed)
		}
		raw, err = dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SlotsSearchCapabilityID,
			[]byte(`{"space_id":"space-missing","date":"2026-09-02"}`))
		if err != nil {
			t.Fatal(err)
		}
		var searched libraryseattool.SlotSearchResult
		if err := json.Unmarshal(raw, &searched); err != nil {
			t.Fatal(err)
		}
		if len(searched.Slots) != 0 {
			t.Fatalf("unknown space must return empty slots, got %#v", searched)
		}
	})

	t.Run("app isolation", func(t *testing.T) {
		raw, err := dispatcher.InvokeCapability(ctx, readRequest("other-app", "user-1"), SpacesListCapabilityID, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var listed libraryseattool.SpaceListResult
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatal(err)
		}
		if listed.DataStatus.Revision != "demo-rev-other-app" {
			t.Fatalf("cross-app catalog leaked: %#v", listed)
		}
	})

	t.Run("untrusted expired incomplete catalog", func(t *testing.T) {
		untrusted := demoCatalog(clock, "campus-services")
		stampRevision(&untrusted, "untrusted-rev")
		untrusted.Source = "scraped-copy"
		untrusted.Authoritative = false
		if err := store.ReplaceCatalog(ctx, untrusted); err != nil {
			t.Fatal(err)
		}
		if _, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SpacesListCapabilityID, []byte(`{}`)); !errors.Is(err, contracts.ErrDataUntrusted) {
			t.Fatalf("untrusted err=%v", err)
		}
		expired := demoCatalog(clock, "campus-services")
		stampRevision(&expired, "expired-rev")
		expired.ImportedAt = clock.Add(-2 * time.Hour)
		expired.ValidUntil = clock.Add(-time.Minute)
		if err := store.ReplaceCatalog(ctx, expired); err != nil {
			t.Fatal(err)
		}
		if _, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SpacesListCapabilityID, []byte(`{}`)); !errors.Is(err, contracts.ErrDataExpired) {
			t.Fatalf("expired err=%v", err)
		}
		if err := store.ReplaceCatalog(ctx, catalog); err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceCatalog(ctx, libraryseattool.CatalogSnapshot{
			AppID: "campus-services", Revision: "incomplete", Source: libraryseattool.DemoSource,
		}); err == nil {
			t.Fatal("incomplete catalog must be rejected at import")
		}
	})

	t.Run("create cancel mine double-book quota cross-user idempotent", func(t *testing.T) {
		createBody := []byte(`{"space_id":"space-demo-main-3f","seat_id":"space-demo-main-3f-A01","slot_id":"slot-morning","date":"2026-09-02"}`)
		created, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "create-1", "confirm-1"), ReservationsCreateCapabilityID, createBody)
		if err != nil {
			t.Fatal(err)
		}
		var first libraryseattool.ReservationResult
		if err := json.Unmarshal(created, &first); err != nil {
			t.Fatal(err)
		}
		if first.Reservation.Status != libraryseattool.ReservationConfirmed || first.DataStatus.State != libraryseattool.DataStateDemoNonAuthoritative {
			t.Fatalf("created=%#v", first)
		}
		searchRaw, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), SlotsSearchCapabilityID,
			[]byte(`{"space_id":"space-demo-main-3f","date":"2026-09-02","slot_id":"slot-morning"}`))
		if err != nil {
			t.Fatal(err)
		}
		var searched libraryseattool.SlotSearchResult
		if err := json.Unmarshal(searchRaw, &searched); err != nil {
			t.Fatal(err)
		}
		if len(searched.Slots) != 1 {
			t.Fatalf("slots=%#v", searched.Slots)
		}
		foundReserved := false
		for _, seat := range searched.Slots[0].Seats {
			if seat.SeatID == "space-demo-main-3f-A01" && seat.Status == libraryseattool.SeatStatusReserved {
				foundReserved = true
			}
		}
		if !foundReserved {
			t.Fatalf("reserved seat missing: %#v", searched.Slots[0].Seats)
		}
		if _, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-2", "create-2", "confirm-2"), ReservationsCreateCapabilityID, createBody); !errors.Is(err, libraryseattool.ErrConflict) {
			t.Fatalf("double-book err=%v", err)
		}
		secondBody := []byte(`{"space_id":"space-demo-main-3f","seat_id":"space-demo-main-3f-A02","slot_id":"slot-morning","date":"2026-09-02"}`)
		if _, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "create-3", "confirm-3"), ReservationsCreateCapabilityID, secondBody); err != nil {
			t.Fatal(err)
		}
		thirdBody := []byte(`{"space_id":"space-demo-main-3f","seat_id":"space-demo-main-3f-B01","slot_id":"slot-morning","date":"2026-09-02"}`)
		if _, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "create-4", "confirm-4"), ReservationsCreateCapabilityID, thirdBody); !errors.Is(err, libraryseattool.ErrQuotaExceeded) {
			t.Fatalf("quota err=%v", err)
		}
		replay, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "create-1", "confirm-1"), ReservationsCreateCapabilityID, createBody)
		if err != nil {
			t.Fatal(err)
		}
		var replayed libraryseattool.ReservationResult
		if err := json.Unmarshal(replay, &replayed); err != nil {
			t.Fatal(err)
		}
		if replayed.Reservation.ID != first.Reservation.ID {
			t.Fatalf("idempotent create drifted %s vs %s", replayed.Reservation.ID, first.Reservation.ID)
		}
		conflictReq := writeRequest("campus-services", "user-1", "create-1", "confirm-1")
		if _, err := dispatcher.InvokeCapability(ctx, conflictReq, ReservationsCreateCapabilityID, secondBody); !errors.Is(err, idempotency.ErrKeyConflict) {
			t.Fatalf("conflicting idempotency err=%v", err)
		}
		cancelBody, _ := json.Marshal(map[string]string{"reservation_id": first.Reservation.ID})
		if _, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-2", "cancel-x", "confirm-x"), ReservationsCancelCapabilityID, cancelBody); !errors.Is(err, libraryseattool.ErrNotOwner) {
			t.Fatalf("cross-user cancel err=%v", err)
		}
		cancelled, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "cancel-1", "confirm-c1"), ReservationsCancelCapabilityID, cancelBody)
		if err != nil {
			t.Fatal(err)
		}
		var cancelResult libraryseattool.ReservationResult
		if err := json.Unmarshal(cancelled, &cancelResult); err != nil || cancelResult.Reservation.Status != libraryseattool.ReservationCancelled {
			t.Fatalf("cancel=%s err=%v", cancelled, err)
		}
		replayCancel, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-1", "cancel-1", "confirm-c1"), ReservationsCancelCapabilityID, cancelBody)
		if err != nil {
			t.Fatal(err)
		}
		var replayCancelResult libraryseattool.ReservationResult
		if err := json.Unmarshal(replayCancel, &replayCancelResult); err != nil || replayCancelResult.Reservation.Status != libraryseattool.ReservationCancelled {
			t.Fatalf("idempotent cancel=%s err=%v", replayCancel, err)
		}
		mineRaw, err := dispatcher.InvokeCapability(ctx, readRequest("campus-services", "user-1"), ReservationsMineCapabilityID, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var mine libraryseattool.MineResult
		if err := json.Unmarshal(mineRaw, &mine); err != nil || len(mine.Reservations) != 2 {
			t.Fatalf("mine=%#v err=%v", mine, err)
		}
	})

	t.Run("confirmation and user required on writes", func(t *testing.T) {
		body := []byte(`{"space_id":"space-demo-main-3f","seat_id":"space-demo-main-3f-B02","slot_id":"slot-afternoon","date":"2026-09-02"}`)
		unconfirmed := writeRequest("campus-services", "user-2", "create-noconfirm", "")
		unconfirmed.ConfirmationID = ""
		if _, err := dispatcher.InvokeCapability(ctx, unconfirmed, ReservationsCreateCapabilityID, body); !errors.Is(err, runtime.ErrConfirmationRequired) {
			t.Fatalf("confirmation err=%v", err)
		}
		missingUser := writeRequest("campus-services", "", "create-nouser", "confirm-nouser")
		if _, err := dispatcher.InvokeCapability(ctx, missingUser, ReservationsCreateCapabilityID, body); !errors.Is(err, libraryseattool.ErrUserRequired) {
			t.Fatalf("missing user err=%v", err)
		}
	})

	t.Run("illegal state transition", func(t *testing.T) {
		body := []byte(`{"space_id":"space-demo-info-1f","seat_id":"space-demo-info-1f-A01","slot_id":"slot-morning","date":"2026-09-02"}`)
		created, err := dispatcher.InvokeCapability(ctx, writeRequest("campus-services", "user-2", "create-expire", "confirm-expire"), ReservationsCreateCapabilityID, body)
		if err != nil {
			t.Fatal(err)
		}
		var result libraryseattool.ReservationResult
		if err := json.Unmarshal(created, &result); err != nil {
			t.Fatal(err)
		}
		late := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC) // 上海 16:00，上午时段已结束
		_, err = store.CancelReservation(ctx, libraryseattool.CancelReservationInput{
			AppID: "campus-services", UserID: "user-2", ReservationID: result.Reservation.ID,
		}, late)
		if !errors.Is(err, libraryseattool.ErrIllegalTransition) {
			t.Fatalf("expired cancel err=%v", err)
		}
	})
}

func stampRevision(snapshot *libraryseattool.CatalogSnapshot, revision string) {
	snapshot.Revision = revision
	for i := range snapshot.Spaces {
		snapshot.Spaces[i].SourceRevision = revision
	}
	for i := range snapshot.Seats {
		snapshot.Seats[i].SourceRevision = revision
	}
	for i := range snapshot.Slots {
		snapshot.Slots[i].SourceRevision = revision
	}
}

func demoCatalog(now time.Time, appID string) libraryseattool.CatalogSnapshot {
	revision := "demo-rev-" + appID
	spaces := []libraryseattool.Space{
		{ID: "space-demo-main-3f", Name: "演示·总馆三楼阅览室", Campus: "文理学部", Building: "总图书馆", Floor: "3F", SourceRevision: revision},
		{ID: "space-demo-info-1f", Name: "演示·信息分馆一楼", Campus: "信息学部", Building: "信息分馆", Floor: "1F", SourceRevision: revision},
	}
	seats := []libraryseattool.Seat{}
	for _, space := range spaces {
		for _, label := range []string{"A01", "A02", "B01", "B02"} {
			seats = append(seats, libraryseattool.Seat{
				ID: space.ID + "-" + label, SpaceID: space.ID, Label: label, Area: label[:1], SourceRevision: revision,
			})
		}
	}
	return libraryseattool.CatalogSnapshot{
		AppID: appID, Revision: revision, Source: libraryseattool.DemoSource,
		Authoritative: false, Complete: true, ImportedAt: now, ValidUntil: now.Add(48 * time.Hour),
		Spaces: spaces, Seats: seats,
		Slots: []libraryseattool.Slot{
			{ID: "slot-morning", Name: "上午", StartMinute: 8 * 60, EndMinute: 12 * 60, SourceRevision: revision},
			{ID: "slot-afternoon", Name: "下午", StartMinute: 12 * 60, EndMinute: 18 * 60, SourceRevision: revision},
			{ID: "slot-evening", Name: "晚上", StartMinute: 18 * 60, EndMinute: 22 * 60, SourceRevision: revision},
		},
	}
}

func readRequest(appID, userID string) contracts.RequestContext {
	return contracts.RequestContext{AppID: appID, EchoID: "echo-1", RequestID: "req-read", UserID: userID, Deadline: time.Now().Add(time.Minute)}
}

func writeRequest(appID, userID, key, confirmation string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: appID, EchoID: "echo-1", RequestID: "req-" + key, UserID: userID,
		IdempotencyKey: key, ConfirmationID: confirmation, Deadline: time.Now().Add(time.Minute),
	}
}

func mustCreateUser(t *testing.T, store *sqlite.Store, userID string) {
	t.Helper()
	if _, err := store.CreateUser(context.Background(), identity.User{UserID: userID, Status: identity.UserStatusActive}); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
}
