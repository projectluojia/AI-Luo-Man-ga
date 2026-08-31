package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

func TestMigration25UpgradesPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v24.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 24); err != nil {
		t.Fatal(err)
	}
	var present int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='classroom_rooms'`).Scan(&present); err != nil || present != 0 {
		t.Fatalf("migration 24 不应包含 classroom_rooms present=%d err=%v", present, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err := upgraded.db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != currentSchemaVersion() {
		t.Fatalf("version=%d current=%d err=%v", version, currentSchemaVersion(), err)
	}
	if version < 25 {
		t.Fatalf("升级后版本=%d，期望至少 25", version)
	}
	if err := upgraded.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('classroom_source_revisions','classroom_current_snapshots','classroom_campuses','classroom_buildings','classroom_rooms','classroom_occupancy','classroom_schedules')`).Scan(&present); err != nil || present != 7 {
		t.Fatalf("migration 25 表数量=%d err=%v", present, err)
	}
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)
	if err := upgraded.ReplaceClassroomSnapshot(t.Context(), ClassroomSnapshot{
		AppID: "campus-services", Revision: "rev-upgrade", Source: "test-authoritative-fixture",
		Authoritative: true, Complete: true, ImportedAt: now, ValidUntil: now.Add(time.Hour),
		Campuses:  []classroom.Campus{{ID: "campus-wenli", Name: "文理学部"}},
		Buildings: []classroom.Building{{ID: "bld-wenli-jiao5", CampusID: "campus-wenli", Name: "教五"}},
		Rooms:     []classroom.Room{{ID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-102", Type: "普通教室", Capacity: 80}},
	}); err != nil {
		t.Fatalf("升级后写入教室快照失败: %v", err)
	}
	rooms, err := upgraded.SearchRooms(t.Context(), "campus-services", classroom.SearchRequest{
		Date: "2026-08-31", CampusID: "campus-wenli", Period: 1, Limit: 10,
	})
	if err != nil || len(rooms.Rooms) != 1 || rooms.Rooms[0].ID != "room-wenli-jiao5-102" {
		t.Fatalf("升级后查询=%#v err=%v", rooms, err)
	}
}

func TestClassroomIncompleteSnapshotFailsClosedOnRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "classroom-incomplete.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)
	if err := store.ReplaceClassroomSnapshot(t.Context(), ClassroomSnapshot{
		AppID: "campus-services", Revision: "rev-inc", Source: "test-authoritative-fixture",
		Authoritative: true, Complete: true, ImportedAt: now, ValidUntil: now.Add(time.Hour),
		Campuses:  []classroom.Campus{{ID: "campus-wenli", Name: "文理学部"}},
		Buildings: []classroom.Building{{ID: "bld-wenli-jiao5", CampusID: "campus-wenli", Name: "教五"}},
		Rooms:     []classroom.Room{{ID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-102", Type: "普通教室", Capacity: 80}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE classroom_source_revisions SET complete=0 WHERE app_id='campus-services'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ListCampuses(t.Context(), "campus-services")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Metadata.Complete {
		t.Fatal("期望读取到不完整快照元数据")
	}
	if _, err := snapshot.Metadata.Govern(now); !errors.Is(err, contracts.ErrDataIncomplete) {
		t.Fatalf("incomplete govern err=%v", err)
	}
}
