package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationsUpgradeRealV9FixtureThroughV14(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v9.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 9); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO echoes(echo_id,app_id,input_message,status,created_at,next_event_sequence)
VALUES('echo','app','input','running',?,1);
INSERT INTO runs(
  app_id,run_id,echo_id,parent_run_id,attempt,status,model,model_config_version,
  protocol_version,max_steps,deadline_at,last_agent_sequence,recoverable_state,
  error_code,error_message,created_at,started_at,completed_at,
  max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,
  max_cost_microusd,provider_timeout_ms,used_input_tokens,used_output_tokens,
  used_total_tokens,used_cost_microusd
) VALUES(
  'app','root','echo',NULL,1,'queued','model','config','1.0',4,?,0,'{}','','',?,NULL,NULL,
  4,1000,500,1500,4096,0,5000,0,0,0,0
);
INSERT INTO capability_audit(
  app_id,call_id,echo_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at
) VALUES('app','call','echo','capability','{}',1,'','',1,?);`,
		now, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), now, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBackup(t.Context(), path); err != nil {
		t.Fatalf("迁移 9 备份不可恢复：%v", err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	runs, err := upgraded.ListRuns(context.Background(), "app", "echo")
	if err != nil || len(runs) != 1 || runs[0].ID != "root" || runs[0].RunGroupID != "root" ||
		runs[0].UsedProviderRetries != 0 || len(runs[0].CapabilityScope) != 0 || len(runs[0].PermissionScope) != 0 {
		t.Fatalf("upgraded runs=%#v err=%v", runs, err)
	}
	audits, err := upgraded.ListCapabilityCalls(context.Background(), "app", "echo")
	if err != nil || len(audits) != 1 || audits[0].RunID != "root" || audits[0].CallID != "call" {
		t.Fatalf("upgraded audits=%#v err=%v", audits, err)
	}
	var version int
	if err := upgraded.db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != currentSchemaVersion() {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestMigrationRegistryVersionOrderAndDuplicateDetection(t *testing.T) {
	saved := registeredMigrations
	registeredMigrations = make(map[int]string)
	defer func() { registeredMigrations = saved }()

	// 注册顺序与版本号无关：版本 18 先注册、版本 15 后注册，仍按 15→18 顺序应用。
	registerMigration(18, `CREATE TABLE registry_18 (id INTEGER PRIMARY KEY);`)
	registerMigration(15, `CREATE TABLE registry_15 (id INTEGER PRIMARY KEY);`)
	if version := currentSchemaVersion(); version != 18 {
		t.Fatalf("current schema version = %d, want 18", version)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("重复注册迁移版本必须显式失败")
			}
		}()
		registerMigration(15, `CREATE TABLE duplicate_15 (id INTEGER PRIMARY KEY);`)
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("非法迁移版本号必须显式失败")
			}
		}()
		registerMigration(0, `CREATE TABLE invalid (id INTEGER PRIMARY KEY);`)
	}()

	applyRegistry := func(t *testing.T, maximumVersion int) (*Store, error) {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "registry.db"))
		if err != nil {
			t.Fatal(err)
		}
		store := &Store{db: db}
		err = store.migrateThrough(t.Context(), maximumVersion)
		if err != nil {
			db.Close()
			return nil, err
		}
		t.Cleanup(func() { db.Close() })
		return store, nil
	}

	full, err := applyRegistry(t, 0)
	if err != nil {
		t.Fatalf("应用全部注册迁移：%v", err)
	}
	var tables, maxVersion int
	if err := full.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('registry_15','registry_18')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("注册迁移后表数量=%d err=%v", tables, err)
	}
	if err := full.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&maxVersion); err != nil || maxVersion != 18 {
		t.Fatalf("记录的最大迁移版本=%d err=%v", maxVersion, err)
	}

	partial, err := applyRegistry(t, 15)
	if err != nil {
		t.Fatalf("部分升级到版本 15：%v", err)
	}
	if err := partial.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('registry_15','registry_18')`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("部分升级后表数量=%d err=%v", tables, err)
	}

	if _, err := applyRegistry(t, 999); err == nil {
		t.Fatal("超过当前 Schema 版本的部分升级必须失败")
	}
	if _, err := applyRegistry(t, -1); err == nil {
		t.Fatal("负的最大迁移版本必须失败")
	}
}

func TestMigrationV14UpgradesPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v13.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 13); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBackup(t.Context(), path); err != nil {
		t.Fatalf("迁移 13 备份不可恢复：%v", err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	rows, err := upgraded.db.QueryContext(t.Context(), `
SELECT run_group_id,origin_call_id,capability_scope,permission_scope,result_message
FROM runs LIMIT 0`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = upgraded.db.QueryContext(t.Context(), `SELECT run_id FROM capability_audit LIMIT 0`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}
