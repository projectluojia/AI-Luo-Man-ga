package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

func TestClassroomStoreSearchIsolatesAppsAndFilters(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "classroom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)
	snapshot := authoritativeClassroomSnapshot("campus-services", now)
	if err := store.ReplaceClassroomSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	other := sqlite.ClassroomSnapshot{
		AppID: "other-app", Revision: "rev-other", Source: "test-authoritative-fixture",
		Authoritative: true, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		Campuses:  []classroom.Campus{{ID: "campus-other", Name: "其他校区"}},
		Buildings: []classroom.Building{{ID: "bld-other", CampusID: "campus-other", Name: "其他楼"}},
		Rooms:     []classroom.Room{{ID: "room-other-only", CampusID: "campus-other", BuildingID: "bld-other", Name: "仅另一App", Type: "普通教室", Capacity: 10}},
	}
	if err := store.ReplaceClassroomSnapshot(ctx, other); err != nil {
		t.Fatal(err)
	}

	empty, err := store.SearchRooms(ctx, "campus-services", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", Period: 1, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := roomIDs(empty.Rooms); !containsAll(ids, "room-wenli-jiao5-102", "room-wenli-yijiao-101") || contains(ids, "room-wenli-jiao5-101") {
		t.Fatalf("period 1 rooms=%v", ids)
	}
	building, err := store.SearchRooms(ctx, "campus-services", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Period: 1, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := roomIDs(building.Rooms); !contains(ids, "room-wenli-jiao5-102") || contains(ids, "room-wenli-yijiao-101") {
		t.Fatalf("building filter rooms=%v", ids)
	}
	none, err := store.SearchRooms(ctx, "campus-services", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-yixue", BuildingID: "bld-yixue-yixue", Period: 13, Limit: 50,
	})
	if err != nil || none.Rooms == nil || len(none.Rooms) != 1 || none.Rooms[0].ID != "room-yixue-yixue-401" {
		t.Fatalf("empty-at-period-13=%#v err=%v", none, err)
	}
	if _, err := store.SearchRooms(ctx, "missing-app", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", Period: 1, Limit: 10,
	}); !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("missing snapshot err=%v", err)
	}
	crossCampus, err := store.SearchRooms(ctx, "other-app", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", Period: 1, Limit: 50,
	})
	if err != nil || len(crossCampus.Rooms) != 0 {
		t.Fatalf("app B must not see app A campus rooms: %#v err=%v", crossCampus, err)
	}
	cross, err := store.SearchRooms(ctx, "other-app", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-other", Period: 1, Limit: 50,
	})
	if err != nil || len(cross.Rooms) != 1 || cross.Rooms[0].ID != "room-other-only" {
		t.Fatalf("app B own rooms=%#v err=%v", cross, err)
	}
	home, err := store.SearchRooms(ctx, "campus-services", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", Period: 13, Limit: 50,
	})
	if err != nil || contains(roomIDs(home.Rooms), "room-other-only") {
		t.Fatalf("app A leaked app B rooms: %#v err=%v", home, err)
	}
}

func TestClassroomStoreScheduleIsolationAndStateMachine(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "classroom-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	identityService := identity.NewService(store)
	if _, err := identityService.CreateUser(ctx, "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := identityService.CreateUser(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC)
	if err := store.ReplaceClassroomSnapshot(ctx, authoritativeClassroomSnapshot("campus-services", now)); err != nil {
		t.Fatal(err)
	}
	item := classroom.ScheduleItem{
		AppID: "campus-services", UserID: "user-a", ID: "sched-1",
		RoomID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5",
		RoomName: "教五-102", AcademicDate: "2026-08-31", Period: 5, Title: "自习",
		Status: classroom.ScheduleStatusScheduled, SourceRevision: "rev-auth",
	}
	created, err := store.CreateSchedule(ctx, item)
	if err != nil || created.ID != "sched-1" || created.Status != classroom.ScheduleStatusScheduled {
		t.Fatalf("create=%#v err=%v", created, err)
	}
	replay, err := store.CreateSchedule(ctx, item)
	if err != nil || replay.ID != created.ID || !replay.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("idempotent create=%#v err=%v", replay, err)
	}
	conflict := item
	conflict.ID = "sched-2"
	if _, err := store.CreateSchedule(ctx, conflict); !errors.Is(err, classroom.ErrConflict) {
		t.Fatalf("same slot different id err=%v", err)
	}
	cancelled, err := store.CancelSchedule(ctx, "campus-services", "user-a", "sched-1", now)
	if err != nil || cancelled.Status != classroom.ScheduleStatusCancelled || cancelled.CancelledAt == nil {
		t.Fatalf("cancel=%#v err=%v", cancelled, err)
	}
	replayCancel, err := store.CancelSchedule(ctx, "campus-services", "user-a", "sched-1", now.Add(time.Minute))
	if err != nil || replayCancel.Status != classroom.ScheduleStatusCancelled || !replayCancel.CancelledAt.Equal(*cancelled.CancelledAt) {
		t.Fatalf("idempotent cancel=%#v err=%v", replayCancel, err)
	}
	if _, err := store.CreateSchedule(ctx, item); !errors.Is(err, classroom.ErrIllegalState) {
		t.Fatalf("revive cancelled err=%v", err)
	}
	if _, err := store.CancelSchedule(ctx, "campus-services", "user-a", "missing", now); !errors.Is(err, classroom.ErrNotFound) {
		t.Fatalf("cancel missing err=%v", err)
	}
	otherApp := item
	otherApp.AppID = "other-app"
	otherApp.ID = "sched-b"
	otherApp.AcademicDate = "2026-09-01"
	if _, err := store.CreateSchedule(ctx, otherApp); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListSchedules(ctx, "campus-services", "user-a", classroom.ScheduleListRequest{})
	if err != nil || len(listed) != 1 || listed[0].ID != "sched-1" {
		t.Fatalf("list user-a=%#v err=%v", listed, err)
	}
	crossUser, err := store.ListSchedules(ctx, "campus-services", "user-b", classroom.ScheduleListRequest{})
	if err != nil || len(crossUser) != 0 {
		t.Fatalf("user isolation=%#v err=%v", crossUser, err)
	}
	crossApp, err := store.ListSchedules(ctx, "other-app", "user-a", classroom.ScheduleListRequest{})
	if err != nil || len(crossApp) != 1 || crossApp[0].ID != "sched-b" {
		t.Fatalf("app isolation=%#v err=%v", crossApp, err)
	}
	if _, err := store.CreateSchedule(canceledCtx(), item); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create err=%v", err)
	}
}

func TestClassroomDemoSnapshotIsNonAuthoritative(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "classroom-demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	if err := demo.LoadClassroomData(context.Background(), store, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ListCampuses(context.Background(), "campus-services")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Metadata.Authoritative || snapshot.Metadata.Source != classroom.DemoSource {
		t.Fatalf("demo metadata=%#v", snapshot.Metadata)
	}
	if _, err := snapshot.Metadata.Govern(now); !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("demo govern err=%v", err)
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func authoritativeClassroomSnapshot(appID string, now time.Time) sqlite.ClassroomSnapshot {
	revision := "rev-auth"
	return sqlite.ClassroomSnapshot{
		AppID: appID, Revision: revision, Source: "test-authoritative-fixture",
		Authoritative: true, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		Campuses: []classroom.Campus{
			{ID: "campus-wenli", Name: "文理学部", SourceRevision: revision},
			{ID: "campus-xinxi", Name: "信息学部", SourceRevision: revision},
			{ID: "campus-gongxue", Name: "工学部", SourceRevision: revision},
			{ID: "campus-yixue", Name: "医学部", SourceRevision: revision},
		},
		Buildings: []classroom.Building{
			{ID: "bld-wenli-jiao5", CampusID: "campus-wenli", Name: "教五", SourceRevision: revision},
			{ID: "bld-wenli-yijiao", CampusID: "campus-wenli", Name: "一教", SourceRevision: revision},
			{ID: "bld-xinxi-jisuanji", CampusID: "campus-xinxi", Name: "计算机大楼", SourceRevision: revision},
			{ID: "bld-gongxue-gongjiao", CampusID: "campus-gongxue", Name: "工教", SourceRevision: revision},
			{ID: "bld-yixue-yixue", CampusID: "campus-yixue", Name: "医学楼", SourceRevision: revision},
		},
		Rooms: []classroom.Room{
			{ID: "room-wenli-jiao5-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-101", Type: "多媒体教室", Capacity: 120, Floor: "3", SourceRevision: revision},
			{ID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-102", Type: "普通教室", Capacity: 80, Floor: "3", SourceRevision: revision},
			{ID: "room-wenli-yijiao-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-yijiao", Name: "一教-101", Type: "多媒体教室", Capacity: 200, Floor: "1", SourceRevision: revision},
			{ID: "room-xinxi-jisuanji-201", CampusID: "campus-xinxi", BuildingID: "bld-xinxi-jisuanji", Name: "计算机大楼-201", Type: "机房", Capacity: 90, Floor: "2", SourceRevision: revision},
			{ID: "room-gongxue-gongjiao-301", CampusID: "campus-gongxue", BuildingID: "bld-gongxue-gongjiao", Name: "工教-301", Type: "普通教室", Capacity: 100, Floor: "3", SourceRevision: revision},
			{ID: "room-yixue-yixue-401", CampusID: "campus-yixue", BuildingID: "bld-yixue-yixue", Name: "医学楼-401", Type: "多媒体教室", Capacity: 80, Floor: "4", SourceRevision: revision},
		},
		Occupancy: []classroom.Occupancy{
			{RoomID: "room-wenli-jiao5-101", AcademicDate: "2026-08-31", Period: 1, SourceRevision: revision},
			{RoomID: "room-wenli-jiao5-101", AcademicDate: "2026-08-31", Period: 2, SourceRevision: revision},
			{RoomID: "room-xinxi-jisuanji-201", AcademicDate: "2026-08-31", Period: 3, SourceRevision: revision},
		},
	}
}

func roomIDs(rooms []classroom.Room) []string {
	ids := make([]string, 0, len(rooms))
	for _, room := range rooms {
		ids = append(ids, room.ID)
	}
	return ids
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAll(values []string, targets ...string) bool {
	for _, target := range targets {
		if !contains(values, target) {
			return false
		}
	}
	return true
}
