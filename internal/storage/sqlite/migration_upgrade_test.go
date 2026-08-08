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
	if err := upgraded.db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
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
