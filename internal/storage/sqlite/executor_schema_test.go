package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestCurrentSchemaUsesExecutorFields(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
		if err := rows.Close(); err != nil {
			t.Fatalf("close table %s: %v", table, err)
		}
		for _, column := range forbidden {
			if _, exists := columns[column]; exists {
				t.Fatalf("current table %s still exposes legacy column %s", table, column)
			}
		}
	}
}

func TestExecutorMigrationNormalizesLegacyErrorValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 26); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO echoes(app_id,echo_id,input_message,status,final_message,error_code,error_message,created_at)
VALUES('app','echo','input','failed','','agent_run_failed','Agent Run 执行失败',?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	echo, _, err := store.GetEcho(t.Context(), "app", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if echo.ErrorCode != "executor_failed" || echo.ErrorMessage != "执行者 Run 执行失败" {
		t.Fatalf("legacy error was not normalized: %#v", echo)
	}
}
