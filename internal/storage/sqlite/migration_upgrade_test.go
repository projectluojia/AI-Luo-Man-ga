package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
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

func TestMigrationV21PreservesAppConfigHeads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v20.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 写入一个"控制面调优过"的当前配置修订与 head：迁移 21 不得清除当前指针。
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO app_config_revisions(
  app_id,revision,enabled,model,system_prompt,channel_prompts,timezone,max_steps,max_tool_calls,
  max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,max_cost_microusd,
  provider_timeout_ms,enabled_capabilities,permission_scope,created_at
) VALUES(
  'app-a',lower(hex(randomblob(32))),1,'model-x','管理员调优提示','{"web":"自定义"}','Asia/Shanghai',
  8,8,32768,8192,40960,65536,0,30000,'[]','[]',?
);
INSERT INTO app_config_heads(app_id,revision,generation,created_at,updated_at)
SELECT app_id,revision,7,?,? FROM app_config_revisions WHERE app_id='app-a';`,
		now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var revision string
	var generation int
	if err := upgraded.db.QueryRowContext(t.Context(),
		`SELECT h.revision,h.generation FROM app_config_heads h WHERE h.app_id='app-a'`).
		Scan(&revision, &generation); err != nil {
		t.Fatalf("升级后 head 丢失（迁移 21 破坏性删除）：%v", err)
	}
	if generation != 7 {
		t.Fatalf("升级后 generation=%d，期望 7", generation)
	}
	var channelPrompts string
	if err := upgraded.db.QueryRowContext(t.Context(),
		`SELECT channel_prompts FROM app_config_revisions WHERE app_id='app-a' AND revision=?`, revision).
		Scan(&channelPrompts); err != nil || channelPrompts != `{"web":"自定义"}` {
		t.Fatalf("升级后渠道提示=%q err=%v", channelPrompts, err)
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

func TestMigrationV25MigratesBusDataToPackageDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-v24.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateThrough(t.Context(), 24); err != nil {
		t.Fatal(err)
	}
	// 在 v24 形态写入 bus 关系数据（含别名与快照指针），随后升级到 v25。
	// 时间串固定为毫秒精度：v13 触发器用 julianday 校验，纳秒精度不可解析。
	const departureAt = "2026-08-30T08:00:00.000Z"
	const arrivalAt = "2026-08-30T08:20:00.000Z"
	if _, err := db.ExecContext(t.Context(), `
INSERT INTO bus_source_revisions(app_id,revision,source,authoritative,imported_at,valid_until,complete)
VALUES('app','revision-1','zhihui-luojia',1,'2026-08-30T07:00:00.000Z','2026-08-30T09:00:00.000Z',1);
INSERT INTO bus_stops(app_id,id,name,aliases,latitude,longitude,source_revision)
VALUES('app','stop-a','文理学部','文理学部站'||char(31)||'本部',30.5,114.3,'revision-1'),
      ('app','stop-b','信息学部','信息学部站',30.5,114.4,'revision-1');
INSERT INTO bus_routes(app_id,id,name,direction,origin_stop_id,destination_stop_id,source_revision)
VALUES('app','route-a','文理—信息','outbound','stop-a','stop-b','revision-1');
INSERT INTO bus_journeys(app_id,trip_id,route_id,route_name,direction,origin_stop_id,origin_stop_name,destination_stop_id,destination_stop_name,departure_at,arrival_at,source_revision)
VALUES('app','trip-a','route-a','文理—信息','outbound','stop-a','文理学部','stop-b','信息学部',?,?,'revision-1');
INSERT INTO bus_current_snapshots(app_id,revision,activated_at)
VALUES('app','revision-1','2026-08-30T07:00:00.000Z');`,
		departureAt, arrivalAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	ctx := context.Background()
	docs := upgraded.PackageDocuments()
	scope := packstore.Scope{AppID: "app", Namespace: "campus/bus"}
	// 快照元数据随 namespace 迁移并保持 current（经 List 一致性读出）。
	listed, err := docs.List(ctx, scope, "routes", 10, "")
	if err != nil || !listed.MetaFound || listed.Meta.Revision != "revision-1" ||
		!listed.Meta.Authoritative || !listed.Meta.Complete {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	// 三张关系表迁为三组文档（两站一线一班）；aliases 由 \x1f 分隔串还原为 JSON 数组。
	var count int
	if err := upgraded.db.QueryRowContext(ctx,
		`SELECT count(*) FROM package_documents WHERE app_id='app' AND namespace='campus/bus'`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("migrated documents=%d err=%v", count, err)
	}
	stop, err := docs.Get(ctx, scope, "stops", "stop-a")
	if err != nil || !stop.Found {
		t.Fatalf("read=%#v err=%v", stop, err)
	}
	if !strings.Contains(string(stop.Document.Payload), `"aliases":["文理学部站","本部"]`) {
		t.Fatalf("stop payload aliases not normalized: %s", stop.Document.Payload)
	}
	if len(listed.Documents) != 1 || listed.Documents[0].ID != "route-a" {
		t.Fatalf("routes=%#v", listed.Documents)
	}
	// bus 专属表已删除。
	for _, table := range []string{"bus_stops", "bus_routes", "bus_journeys", "bus_source_revisions", "bus_current_snapshots"} {
		if err := upgraded.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("table %s still exists: count=%d err=%v", table, count, err)
		}
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
