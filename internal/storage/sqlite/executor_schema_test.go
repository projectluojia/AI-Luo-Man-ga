package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshSchemaUsesExecutorFields(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaBaselineVersion {
		t.Fatalf("schema version=%d, want %d", version, schemaBaselineVersion)
	}
	for table, forbidden := range map[string][]string{
		"app_config_revisions": {"model", "system_prompt", "channel_prompts", "provider_timeout_ms"},
		"runs":                 {"model", "model_config_version", "system_prompt", "provider_timeout_ms", "last_agent_sequence", "max_input_tokens", "max_output_tokens", "max_total_tokens"},
	} {
		columns := make(map[string]struct{})
		rows, err := store.db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatalf("scan table %s: %v", table, err)
			}
			columns[name] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate table %s: %v", table, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close table %s: %v", table, err)
		}
		for _, column := range forbidden {
			if _, exists := columns[column]; exists {
				t.Fatalf("current table %s still exposes legacy column %s", table, column)
			}
		}
	}
	var legacyTables int
	if err := store.db.QueryRow(`
SELECT count(*) FROM sqlite_master
WHERE type='table' AND name IN ('user_prompt_settings','bus_source_revisions','bus_stops','bus_routes','bus_journeys','bus_current_snapshots')`).Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 0 {
		t.Fatalf("fresh schema still exposes legacy tables: %d", legacyTables)
	}
}

func TestOpenRejectsPreBaselineDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO schema_migrations(version, applied_at) VALUES(27, '2026-09-01T00:00:00Z');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "删除数据库并重新部署") {
		t.Fatalf("legacy database error=%v", err)
	}
}
