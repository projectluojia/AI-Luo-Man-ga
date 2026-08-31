package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigration26UpgradesFromPreviousVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v24.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 24); err != nil {
		t.Fatal(err)
	}
	if _, err := db.QueryContext(t.Context(), `SELECT 1 FROM library_seat_reservations LIMIT 0`); err == nil {
		t.Fatal("v24 不得包含图书馆座位表")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := upgraded.Close(); err != nil {
			t.Errorf("close upgraded store: %v", err)
		}
	}()
	var version int
	if err := upgraded.db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion() || version < 26 {
		t.Fatalf("version=%d current=%d", version, currentSchemaVersion())
	}
	for _, table := range []string{
		"library_seat_source_revisions",
		"library_seat_current_snapshots",
		"library_seat_spaces",
		"library_seat_seats",
		"library_seat_slots",
		"library_seat_reservations",
	} {
		rows, err := upgraded.db.QueryContext(t.Context(), "SELECT 1 FROM "+table+" LIMIT 0")
		if err != nil {
			t.Fatalf("升级后缺少表 %s: %v", table, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
