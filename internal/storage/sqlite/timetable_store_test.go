package sqlite_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	timetable "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/timetable"
)

func openTimetableStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "timetable.db"))
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func createTimetableUser(t *testing.T, store *sqlite.Store, userID string) {
	t.Helper()
	if _, err := identity.NewService(store).CreateUser(t.Context(), userID); err != nil {
		t.Fatal(err)
	}
}

func testCourse(appID, userID, tableID, courseID string) timetable.Course {
	return timetable.Course{AppID: appID, UserID: userID, TimetableID: tableID, ID: courseID, Title: "高等数学", Weekday: 2, ClassFrom: 1, ClassTo: 2, Weeks: []int{1, 3, 5}, Instructor: "张老师"}
}

func TestTimetableStoreIsolatesAppAndUserAndKeepsOneActive(t *testing.T) {
	store, dir := openTimetableStore(t)
	defer sqlitetest.CloseAndWait(t, store, dir)
	createTimetableUser(t, store, "alice")
	createTimetableUser(t, store, "bob")
	first, err := store.CreateTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: "one", Name: "第一张", Source: timetable.SourceLocal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: "two", Name: "第二张", Source: timetable.SourceLocal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	currentFirst, err := store.GetTimetable(t.Context(), "app-a", "alice", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentFirst.Active || !second.Active {
		t.Fatalf("active state first=%v second=%v", currentFirst.Active, second.Active)
	}
	if _, err := store.GetTimetable(t.Context(), "app-a", "bob", "one"); !errors.Is(err, timetable.ErrNotFound) {
		t.Fatalf("cross-user lookup err=%v", err)
	}
	if _, err := store.GetTimetable(t.Context(), "app-b", "alice", "one"); !errors.Is(err, timetable.ErrNotFound) {
		t.Fatalf("cross-app lookup err=%v", err)
	}
	if _, err := store.CreateCourse(t.Context(), testCourse("app-a", "alice", "two", "course-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetCourse(t.Context(), "app-a", "alice", "one", "course-1"); !errors.Is(err, timetable.ErrNotFound) {
		t.Fatalf("course crossed timetable err=%v", err)
	}
}

func TestTimetableImportIsAtomicAndValidatesCapacity(t *testing.T) {
	store, dir := openTimetableStore(t)
	defer sqlitetest.CloseAndWait(t, store, dir)
	createTimetableUser(t, store, "alice")
	valid := testCourse("app-a", "alice", "imported", "course-1")
	invalid := valid
	invalid.ID = "course-2"
	invalid.Weeks = nil
	if _, _, err := store.ImportTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: "imported", Name: "导入", Source: timetable.SourceWUDA}, []timetable.Course{valid, invalid}); !errors.Is(err, timetable.ErrInvalid) {
		t.Fatalf("invalid import err=%v", err)
	}
	if _, err := store.GetTimetable(t.Context(), "app-a", "alice", "imported"); !errors.Is(err, timetable.ErrNotFound) {
		t.Fatalf("failed import left timetable: %v", err)
	}
	created, courses, err := store.ImportTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: "imported", Name: "导入", Source: timetable.SourceWUDA, Active: true}, []timetable.Course{valid})
	if err != nil || created.Active != true || len(courses) != 1 {
		t.Fatalf("valid import created=%#v courses=%#v err=%v", created, courses, err)
	}
	if got, err := store.ListCourses(t.Context(), "app-a", "alice", "imported"); err != nil || len(got) != 1 {
		t.Fatalf("stored courses=%#v err=%v", got, err)
	}
}

func TestTimetableStoreEnforcesPerUserTimetableCapacity(t *testing.T) {
	store, dir := openTimetableStore(t)
	defer sqlitetest.CloseAndWait(t, store, dir)
	createTimetableUser(t, store, "alice")
	for i := 0; i < timetable.MaxTimetablesPerUser; i++ {
		id := fmt.Sprintf("table-%d", i)
		if _, err := store.CreateTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: id, Name: id, Source: timetable.SourceLocal}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := store.CreateTimetable(t.Context(), timetable.Timetable{AppID: "app-a", UserID: "alice", ID: "overflow", Name: "overflow", Source: timetable.SourceLocal}); !errors.Is(err, timetable.ErrCapacity) {
		t.Fatalf("capacity err=%v", err)
	}
}
