package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type Store struct {
	db *sql.DB
	// txMu 按事务生命周期串行化全部写事务：modernc/sqlite 单连接并发事务会破坏
	// 事务边界（嵌套 BEGIN / 提交时语句进行中 / 并发语句报锁），事务必须显式互斥。
	txMu sync.Mutex
}

type rowScanner interface {
	Scan(...any) error
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type queryer interface {
	rowQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// beginTx 领取事务并持有串行化互斥，直到 finishTx 提交/回滚后释放。
func (s *Store) beginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	s.txMu.Lock()
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		s.txMu.Unlock()
		return nil, err
	}
	return tx, nil
}

// finishTx 结束事务并释放串行化互斥（回滚逻辑复用 rollbackTx）。
func (s *Store) finishTx(tx *sql.Tx, resultErr *error, operation string) {
	*resultErr = s.rollbackTx(tx, *resultErr, operation)
}

// rollbackTx 回滚事务并释放串行化互斥（beginTx 后的内联错误路径专用）。
func (s *Store) rollbackTx(tx *sql.Tx, primary error, operation string) error {
	err := rollbackTransaction(tx, primary, operation)
	s.txMu.Unlock()
	return err
}

func Open(path string) (*Store, error) {
	started := time.Now()
	// 单连接 + busy_timeout：连接上限与事务互斥（txMu）共同保证串行化；
	// busy_timeout 兜底跨进程写竞争。foreign_keys 经 DSN 对每个新连接生效。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	observe.Info(context.Background(), "SQLite 统一数据库已经打开",
		observe.Component("storage"),
		observe.Duration(started),
	)
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "ping", started, resultErr) }()
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	return s.migrateThrough(ctx, 0)
}

// registeredMigrations 是统一的前向迁移注册表，key 为唯一迁移版本号。
// 各存储实现通过 registerMigration 在包初始化阶段注册自身迁移，
// 使多个模块可以独立并行地扩展数据库 Schema，而不需要改动既有迁移。
var registeredMigrations = make(map[int]string)

// registerMigration 注册一个前向迁移。版本号必须唯一且大于 0；
// 重复或非法注册属于启动期编程错误，直接以显式错误终止启动。
func registerMigration(version int, statements string) {
	if version <= 0 {
		panic(fmt.Sprintf("非法迁移版本号 %d", version))
	}
	if _, exists := registeredMigrations[version]; exists {
		panic(fmt.Sprintf("迁移版本 %d 重复注册", version))
	}
	registeredMigrations[version] = statements
}

// currentSchemaVersion 返回当前注册的最大迁移版本，即当前数据库 Schema 版本。
func currentSchemaVersion() int {
	maximum := 0
	for version := range registeredMigrations {
		if version > maximum {
			maximum = version
		}
	}
	return maximum
}

func init() {
	baseMigrations := []string{`
CREATE TABLE IF NOT EXISTS bus_source_revisions (
  app_id TEXT NOT NULL,
  revision TEXT NOT NULL,
  source TEXT NOT NULL,
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0, 1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT,
  PRIMARY KEY (app_id, revision)
);
CREATE TABLE IF NOT EXISTS bus_stops (
  app_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  aliases TEXT NOT NULL DEFAULT '',
  latitude REAL NOT NULL DEFAULT 0,
  longitude REAL NOT NULL DEFAULT 0,
  source_revision TEXT NOT NULL,
  PRIMARY KEY (app_id, id)
);
CREATE INDEX IF NOT EXISTS bus_stops_name_idx ON bus_stops(app_id, name);
CREATE TABLE IF NOT EXISTS bus_routes (
  app_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  direction TEXT NOT NULL,
  origin_stop_id TEXT NOT NULL,
  destination_stop_id TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  PRIMARY KEY (app_id, id)
);
CREATE TABLE IF NOT EXISTS bus_journeys (
  app_id TEXT NOT NULL,
  trip_id TEXT NOT NULL,
  route_id TEXT NOT NULL,
  route_name TEXT NOT NULL,
  direction TEXT NOT NULL,
  origin_stop_id TEXT NOT NULL,
  origin_stop_name TEXT NOT NULL,
  destination_stop_id TEXT NOT NULL,
  destination_stop_name TEXT NOT NULL,
  departure_at TEXT NOT NULL,
  arrival_at TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  PRIMARY KEY (app_id, trip_id, origin_stop_id, destination_stop_id)
);
CREATE INDEX IF NOT EXISTS bus_journeys_search_idx ON bus_journeys(app_id, origin_stop_id, destination_stop_id, departure_at);
CREATE TABLE IF NOT EXISTS echoes (
  echo_id TEXT PRIMARY KEY,
  app_id TEXT NOT NULL,
  input_message TEXT NOT NULL,
  status TEXT NOT NULL,
  final_message TEXT,
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  completed_at TEXT
);
CREATE TABLE IF NOT EXISTS echo_events (
  echo_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  type TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (echo_id, sequence),
  FOREIGN KEY (echo_id) REFERENCES echoes(echo_id)
);
CREATE TABLE IF NOT EXISTS capability_audit (
  call_id TEXT PRIMARY KEY,
  echo_id TEXT NOT NULL,
  app_id TEXT NOT NULL,
  capability_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  success INTEGER NOT NULL,
  error_message TEXT,
  duration_ms INTEGER NOT NULL,
  created_at TEXT NOT NULL
);`, `ALTER TABLE echo_events ADD COLUMN run_id TEXT NOT NULL DEFAULT '';`, `
CREATE TABLE echoes_v3 (
  app_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  input_message TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('running', 'succeeded', 'failed', 'cancelled')),
  final_message TEXT,
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  completed_at TEXT,
  PRIMARY KEY (app_id, echo_id)
);
INSERT INTO echoes_v3(app_id,echo_id,input_message,status,final_message,error_code,error_message,created_at,completed_at)
SELECT app_id,echo_id,input_message,status,final_message,
  CASE coalesce(error_code,'')
    WHEN '' THEN ''
    WHEN 'cancelled' THEN 'cancelled'
    WHEN 'deadline_exceeded' THEN 'deadline_exceeded'
    WHEN 'agent_unavailable' THEN 'agent_unavailable'
    WHEN 'agent_start_failed' THEN 'agent_unavailable'
    WHEN 'agent_stream_failed' THEN 'agent_unavailable'
    WHEN 'protocol_violation' THEN 'protocol_violation'
    WHEN 'agent_run_failed' THEN 'agent_run_failed'
    ELSE 'internal_error'
  END,
  CASE coalesce(error_code,'')
    WHEN '' THEN ''
    WHEN 'cancelled' THEN 'Echo 已取消'
    WHEN 'deadline_exceeded' THEN 'Echo 执行超时'
    WHEN 'agent_unavailable' THEN 'Agent 服务暂时不可用'
    WHEN 'agent_start_failed' THEN 'Agent 服务暂时不可用'
    WHEN 'agent_stream_failed' THEN 'Agent 服务暂时不可用'
    WHEN 'protocol_violation' THEN 'Agent 协议响应无效'
    WHEN 'agent_run_failed' THEN 'Agent Run 执行失败'
    ELSE 'Echo 执行失败'
  END,
  created_at,completed_at FROM echoes;

CREATE TABLE echo_events_v3 (
  app_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  sequence INTEGER NOT NULL CHECK(sequence > 0),
  type TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload)),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, echo_id, sequence),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes_v3(app_id, echo_id) ON DELETE CASCADE
);
INSERT INTO echo_events_v3(app_id,echo_id,run_id,sequence,type,payload,created_at)
SELECT e.app_id,v.echo_id,v.run_id,v.sequence,v.type,v.payload,v.created_at
FROM echo_events v JOIN echoes e ON e.echo_id=v.echo_id;

CREATE TABLE capability_audit_v3 (
  app_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  capability_id TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload)),
  success INTEGER NOT NULL CHECK(success IN (0, 1)),
  error_code TEXT,
  error_message TEXT,
  duration_ms INTEGER NOT NULL CHECK(duration_ms >= 0),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, call_id),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes_v3(app_id, echo_id) ON DELETE CASCADE
);
INSERT INTO capability_audit_v3(app_id,call_id,echo_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at)
SELECT app_id,call_id,echo_id,capability_id,payload,success,
  CASE WHEN success=1 THEN '' ELSE 'capability_failed' END,
  CASE WHEN success=1 THEN '' ELSE 'Capability 调用失败' END,
  duration_ms,created_at FROM capability_audit;

DROP TABLE capability_audit;
DROP TABLE echo_events;
DROP TABLE echoes;
ALTER TABLE echoes_v3 RENAME TO echoes;
ALTER TABLE echo_events_v3 RENAME TO echo_events;
ALTER TABLE capability_audit_v3 RENAME TO capability_audit;
CREATE INDEX echo_events_replay_idx ON echo_events(app_id, echo_id, sequence);
CREATE INDEX capability_audit_echo_idx ON capability_audit(app_id, echo_id, created_at);
`, `
ALTER TABLE echoes ADD COLUMN next_event_sequence INTEGER NOT NULL DEFAULT 1 CHECK(next_event_sequence > 0);
UPDATE echoes
SET next_event_sequence = coalesce((
  SELECT max(sequence) + 1 FROM echo_events
  WHERE echo_events.app_id=echoes.app_id AND echo_events.echo_id=echoes.echo_id
), 1);

CREATE TABLE runs (
  app_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  parent_run_id TEXT,
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled','timed_out')),
  model TEXT NOT NULL,
  model_config_version TEXT NOT NULL,
  protocol_version TEXT NOT NULL,
  max_steps INTEGER NOT NULL CHECK(max_steps >= 0),
  deadline_at TEXT NOT NULL,
  lease_token TEXT,
  lease_expires_at TEXT,
  last_agent_sequence INTEGER NOT NULL DEFAULT 0 CHECK(last_agent_sequence >= 0),
  recoverable_state TEXT NOT NULL CHECK(json_valid(recoverable_state)),
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  PRIMARY KEY (app_id, run_id),
  UNIQUE (app_id, echo_id, attempt),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes(app_id, echo_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, parent_run_id) REFERENCES runs(app_id, run_id),
  CHECK(parent_run_id IS NULL OR parent_run_id <> run_id)
);
CREATE INDEX runs_queue_idx ON runs(app_id, status, created_at);
CREATE INDEX runs_echo_idx ON runs(app_id, echo_id, attempt);
CREATE INDEX runs_lease_idx ON runs(app_id, status, lease_expires_at);

INSERT INTO runs(
  app_id,run_id,echo_id,parent_run_id,attempt,status,model,model_config_version,
  protocol_version,max_steps,deadline_at,last_agent_sequence,recoverable_state,
  error_code,error_message,created_at,started_at,completed_at
)
SELECT
  e.app_id,
  coalesce(nullif((
    SELECT v.run_id FROM echo_events v
    WHERE v.app_id=e.app_id AND v.echo_id=e.echo_id AND v.run_id<>''
    ORDER BY v.sequence DESC LIMIT 1
  ), ''), 'legacy-' || e.echo_id),
  e.echo_id,
  NULL,
  1,
  CASE e.status
    WHEN 'succeeded' THEN 'succeeded'
    WHEN 'cancelled' THEN 'cancelled'
    ELSE 'failed'
  END,
  'legacy-unknown',
  'legacy-v3',
  'legacy-unknown',
  0,
  e.created_at,
  0,
  '{}',
  CASE WHEN e.status='running' THEN 'recovery_failed' ELSE coalesce(e.error_code,'') END,
  CASE WHEN e.status='running' THEN 'Run 无法安全恢复' ELSE coalesce(e.error_message,'') END,
  e.created_at,
  e.created_at,
  coalesce(e.completed_at,e.created_at)
FROM echoes e;

UPDATE echoes
SET status='failed',
    final_message='',
    error_code='recovery_failed',
    error_message='Run 无法安全恢复',
    completed_at=coalesce(completed_at,created_at)
WHERE status='running';
`, `
CREATE TABLE idempotency_records (
  app_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_fingerprint TEXT NOT NULL CHECK(length(request_fingerprint)=64),
  owner_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('executing','succeeded','failed')),
  lease_token TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  result BLOB,
  error_code TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  completed_at TEXT,
  expires_at TEXT,
  PRIMARY KEY (app_id, scope, idempotency_key),
  CHECK(result IS NULL OR length(result) <= 262144),
  CHECK(
    (status='executing' AND result IS NULL AND error_code='' AND completed_at IS NULL AND expires_at IS NULL) OR
    (status='succeeded' AND result IS NOT NULL AND error_code='' AND completed_at IS NOT NULL AND expires_at IS NOT NULL) OR
    (status='failed' AND result IS NULL AND error_code='operation_failed' AND completed_at IS NOT NULL AND expires_at IS NOT NULL)
  )
);
CREATE INDEX idempotency_expiry_idx ON idempotency_records(app_id, expires_at);

CREATE TABLE echo_create_requests (
  app_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_fingerprint TEXT NOT NULL CHECK(length(request_fingerprint)=64),
  echo_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, idempotency_key),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes(app_id, echo_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX echo_create_request_echo_idx ON echo_create_requests(app_id, echo_id);
`, `
ALTER TABLE runs ADD COLUMN max_tool_calls INTEGER NOT NULL DEFAULT 8 CHECK(max_tool_calls > 0);
ALTER TABLE runs ADD COLUMN max_input_tokens INTEGER NOT NULL DEFAULT 32768 CHECK(max_input_tokens > 0);
ALTER TABLE runs ADD COLUMN max_output_tokens INTEGER NOT NULL DEFAULT 8192 CHECK(max_output_tokens > 0);
ALTER TABLE runs ADD COLUMN max_total_tokens INTEGER NOT NULL DEFAULT 40960 CHECK(max_total_tokens > 0);
ALTER TABLE runs ADD COLUMN max_output_bytes INTEGER NOT NULL DEFAULT 65536 CHECK(max_output_bytes > 0);
ALTER TABLE runs ADD COLUMN max_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK(max_cost_microusd >= 0);
ALTER TABLE runs ADD COLUMN provider_timeout_ms INTEGER NOT NULL DEFAULT 30000 CHECK(provider_timeout_ms > 0);
ALTER TABLE runs ADD COLUMN used_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_input_tokens >= 0);
ALTER TABLE runs ADD COLUMN used_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_output_tokens >= 0);
ALTER TABLE runs ADD COLUMN used_total_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_total_tokens >= 0);
ALTER TABLE runs ADD COLUMN used_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK(used_cost_microusd >= 0);
`, `
CREATE TRIGGER runs_budget_insert_guard
BEFORE INSERT ON runs
WHEN NEW.max_steps > 64 OR NEW.max_tool_calls > 256 OR
     NEW.max_input_tokens > 1000000000 OR NEW.max_output_tokens > 1000000000 OR
     NEW.max_total_tokens > 1000000000 OR NEW.max_output_bytes > 65536 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.provider_timeout_ms < 100 OR NEW.provider_timeout_ms > 120000 OR
     NEW.used_total_tokens != NEW.used_input_tokens + NEW.used_output_tokens OR
     NEW.used_input_tokens > NEW.max_input_tokens OR
     NEW.used_output_tokens > NEW.max_output_tokens OR
     NEW.used_total_tokens > NEW.max_total_tokens OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT, 'invalid run budget');
END;

CREATE TRIGGER runs_budget_update_guard
BEFORE UPDATE OF max_steps,max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,
                 max_output_bytes,max_cost_microusd,provider_timeout_ms,
                 used_input_tokens,used_output_tokens,used_total_tokens,used_cost_microusd ON runs
WHEN NEW.max_steps > 64 OR NEW.max_tool_calls > 256 OR
     NEW.max_input_tokens > 1000000000 OR NEW.max_output_tokens > 1000000000 OR
     NEW.max_total_tokens > 1000000000 OR NEW.max_output_bytes > 65536 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.provider_timeout_ms < 100 OR NEW.provider_timeout_ms > 120000 OR
     NEW.used_total_tokens != NEW.used_input_tokens + NEW.used_output_tokens OR
     NEW.used_input_tokens > NEW.max_input_tokens OR
     NEW.used_output_tokens > NEW.max_output_tokens OR
     NEW.used_total_tokens > NEW.max_total_tokens OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT, 'invalid run budget');
END;
`, `
CREATE TABLE bus_current_snapshots (
  app_id TEXT PRIMARY KEY,
  revision TEXT NOT NULL,
  activated_at TEXT NOT NULL,
  FOREIGN KEY (app_id, revision) REFERENCES bus_source_revisions(app_id, revision) ON DELETE RESTRICT
);

WITH active_rows(app_id, revision) AS (
  SELECT app_id, source_revision FROM bus_stops
  UNION
  SELECT app_id, source_revision FROM bus_routes
  UNION
  SELECT app_id, source_revision FROM bus_journeys
)
INSERT INTO bus_current_snapshots(app_id, revision, activated_at)
SELECT active_rows.app_id, min(active_rows.revision), strftime('%Y-%m-%dT%H:%M:%fZ','now')
FROM active_rows
JOIN bus_source_revisions source
  ON source.app_id=active_rows.app_id AND source.revision=active_rows.revision
GROUP BY active_rows.app_id
HAVING min(active_rows.revision)=max(active_rows.revision);
`, `
ALTER TABLE bus_source_revisions
ADD COLUMN complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN (0,1));

CREATE TRIGGER bus_stop_revision_insert_guard
BEFORE INSERT ON bus_stops
WHEN NOT EXISTS (
  SELECT 1 FROM bus_source_revisions source
  WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
)
BEGIN
  SELECT RAISE(ABORT, 'invalid bus stop source revision');
END;

CREATE TRIGGER bus_route_reference_insert_guard
BEFORE INSERT ON bus_routes
WHEN NEW.origin_stop_id=NEW.destination_stop_id OR
     NOT EXISTS (
       SELECT 1 FROM bus_source_revisions source
       WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
     ) OR
     NOT EXISTS (
       SELECT 1 FROM bus_stops stop
       WHERE stop.app_id=NEW.app_id AND stop.id=NEW.origin_stop_id
         AND stop.source_revision=NEW.source_revision
     ) OR
     NOT EXISTS (
       SELECT 1 FROM bus_stops stop
       WHERE stop.app_id=NEW.app_id AND stop.id=NEW.destination_stop_id
         AND stop.source_revision=NEW.source_revision
     )
BEGIN
  SELECT RAISE(ABORT, 'invalid bus route references');
END;

CREATE TRIGGER bus_journey_reference_insert_guard
BEFORE INSERT ON bus_journeys
WHEN NEW.arrival_at<=NEW.departure_at OR
     NOT EXISTS (
       SELECT 1 FROM bus_source_revisions source
       WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
     ) OR
     NOT EXISTS (
       SELECT 1 FROM bus_routes route
       WHERE route.app_id=NEW.app_id AND route.id=NEW.route_id
         AND route.source_revision=NEW.source_revision
         AND route.origin_stop_id=NEW.origin_stop_id
         AND route.destination_stop_id=NEW.destination_stop_id
     )
BEGIN
  SELECT RAISE(ABORT, 'invalid bus journey references');
END;
`, `
ALTER TABLE runs
ADD COLUMN used_provider_retries INTEGER NOT NULL DEFAULT 0
CHECK(used_provider_retries >= 0 AND used_provider_retries <= 320);
`, `
ALTER TABLE runs
ADD COLUMN available_at TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z';
UPDATE runs SET available_at=created_at;
CREATE INDEX runs_runnable_idx ON runs(app_id,status,available_at,created_at,run_id);
`, `
DROP TRIGGER bus_journey_reference_insert_guard;
CREATE TRIGGER bus_journey_reference_insert_guard
BEFORE INSERT ON bus_journeys
WHEN julianday(NEW.arrival_at) IS NULL OR julianday(NEW.departure_at) IS NULL OR
     julianday(NEW.arrival_at)<=julianday(NEW.departure_at) OR
     NOT EXISTS (
       SELECT 1 FROM bus_source_revisions source
       WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
     ) OR
     NOT EXISTS (
       SELECT 1 FROM bus_routes route
       WHERE route.app_id=NEW.app_id AND route.id=NEW.route_id
         AND route.source_revision=NEW.source_revision
         AND route.origin_stop_id=NEW.origin_stop_id
         AND route.destination_stop_id=NEW.destination_stop_id
     )
BEGIN
  SELECT RAISE(ABORT, 'invalid bus journey references');
END;
`, `
CREATE TABLE app_config_revisions (
  app_id TEXT NOT NULL,
  revision TEXT NOT NULL CHECK(length(revision)=64),
  enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
  model TEXT NOT NULL CHECK(length(model) BETWEEN 1 AND 256),
  system_prompt TEXT NOT NULL CHECK(length(system_prompt) BETWEEN 1 AND 16384 AND instr(system_prompt,char(0))=0),
  timezone TEXT NOT NULL CHECK(length(timezone) BETWEEN 1 AND 128),
  max_steps INTEGER NOT NULL CHECK(max_steps BETWEEN 1 AND 64),
  max_tool_calls INTEGER NOT NULL CHECK(max_tool_calls BETWEEN 1 AND 128),
  max_input_tokens INTEGER NOT NULL CHECK(max_input_tokens BETWEEN 1 AND 10000000),
  max_output_tokens INTEGER NOT NULL CHECK(max_output_tokens BETWEEN 1 AND 1000000),
  max_total_tokens INTEGER NOT NULL CHECK(max_total_tokens BETWEEN max_input_tokens AND 11000000),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes BETWEEN 1 AND 262144),
  max_cost_microusd INTEGER NOT NULL CHECK(max_cost_microusd BETWEEN 0 AND 1000000000000),
  provider_timeout_ms INTEGER NOT NULL CHECK(provider_timeout_ms BETWEEN 100 AND 300000),
  enabled_capabilities TEXT NOT NULL CHECK(length(enabled_capabilities)<=65536 AND json_valid(enabled_capabilities) AND json_type(enabled_capabilities)='array'),
  permission_scope TEXT NOT NULL CHECK(length(permission_scope)<=65536 AND json_valid(permission_scope) AND json_type(permission_scope)='array'),
  created_at TEXT NOT NULL,
  PRIMARY KEY(app_id,revision)
);
CREATE TABLE app_config_heads (
  app_id TEXT NOT NULL PRIMARY KEY,
  revision TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK(generation > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(app_id,revision) REFERENCES app_config_revisions(app_id,revision) ON DELETE RESTRICT
);
CREATE INDEX app_config_revision_created_idx ON app_config_revisions(app_id,created_at,revision);
`, `
DROP TRIGGER runs_budget_insert_guard;
DROP TRIGGER runs_budget_update_guard;

CREATE TABLE runs_v14 (
  app_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_group_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  parent_run_id TEXT,
  origin_call_id TEXT NOT NULL DEFAULT '' CHECK(length(origin_call_id)<=128),
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled','timed_out')),
  model TEXT NOT NULL,
  model_config_version TEXT NOT NULL,
  protocol_version TEXT NOT NULL,
  max_steps INTEGER NOT NULL CHECK(max_steps >= 0),
  max_tool_calls INTEGER NOT NULL CHECK(max_tool_calls > 0),
  max_input_tokens INTEGER NOT NULL CHECK(max_input_tokens > 0),
  max_output_tokens INTEGER NOT NULL CHECK(max_output_tokens > 0),
  max_total_tokens INTEGER NOT NULL CHECK(max_total_tokens > 0),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes > 0),
  max_cost_microusd INTEGER NOT NULL CHECK(max_cost_microusd >= 0),
  provider_timeout_ms INTEGER NOT NULL CHECK(provider_timeout_ms > 0),
  used_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_input_tokens >= 0),
  used_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_output_tokens >= 0),
  used_total_tokens INTEGER NOT NULL DEFAULT 0 CHECK(used_total_tokens >= 0),
  used_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK(used_cost_microusd >= 0),
  used_provider_retries INTEGER NOT NULL DEFAULT 0 CHECK(used_provider_retries >= 0 AND used_provider_retries <= 320),
  deadline_at TEXT NOT NULL,
  available_at TEXT NOT NULL,
  lease_token TEXT,
  lease_expires_at TEXT,
  last_agent_sequence INTEGER NOT NULL DEFAULT 0 CHECK(last_agent_sequence >= 0),
  capability_scope TEXT NOT NULL DEFAULT '[]' CHECK(length(capability_scope)<=65536 AND json_valid(capability_scope) AND json_type(capability_scope)='array'),
  permission_scope TEXT NOT NULL DEFAULT '[]' CHECK(length(permission_scope)<=65536 AND json_valid(permission_scope) AND json_type(permission_scope)='array'),
  recoverable_state TEXT NOT NULL CHECK(json_valid(recoverable_state)),
  result_message TEXT NOT NULL DEFAULT '' CHECK(length(result_message)<=65536),
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  PRIMARY KEY (app_id,run_id),
  UNIQUE (app_id,run_group_id,attempt),
  UNIQUE (app_id,parent_run_id,origin_call_id),
  FOREIGN KEY (app_id,echo_id) REFERENCES echoes(app_id,echo_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id,parent_run_id) REFERENCES runs_v14(app_id,run_id),
  CHECK(parent_run_id IS NULL OR parent_run_id<>run_id),
  CHECK((parent_run_id IS NULL AND origin_call_id='') OR (parent_run_id IS NOT NULL AND origin_call_id<>''))
);

INSERT INTO runs_v14(
  app_id,run_id,run_group_id,echo_id,parent_run_id,origin_call_id,attempt,status,
  model,model_config_version,protocol_version,max_steps,max_tool_calls,
  max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,max_cost_microusd,
  provider_timeout_ms,used_input_tokens,used_output_tokens,used_total_tokens,used_cost_microusd,
  used_provider_retries,deadline_at,available_at,lease_token,lease_expires_at,last_agent_sequence,
  capability_scope,permission_scope,recoverable_state,result_message,error_code,error_message,
  created_at,started_at,completed_at
)
SELECT
  app_id,run_id,run_id,echo_id,parent_run_id,
  CASE WHEN parent_run_id IS NULL THEN '' ELSE 'legacy-' || substr(run_id,1,121) END,
  attempt,status,model,model_config_version,protocol_version,max_steps,max_tool_calls,
  max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,max_cost_microusd,
  provider_timeout_ms,used_input_tokens,used_output_tokens,used_total_tokens,used_cost_microusd,
  used_provider_retries,deadline_at,available_at,lease_token,lease_expires_at,last_agent_sequence,
  '[]','[]',recoverable_state,'',error_code,error_message,created_at,started_at,completed_at
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_v14 RENAME TO runs;
CREATE INDEX runs_queue_idx ON runs(app_id,status,created_at);
CREATE INDEX runs_echo_idx ON runs(app_id,echo_id,run_group_id,attempt);
CREATE INDEX runs_lease_idx ON runs(app_id,status,lease_expires_at);
CREATE INDEX runs_runnable_idx ON runs(app_id,status,available_at,created_at,run_id);
CREATE INDEX runs_parent_idx ON runs(app_id,parent_run_id,created_at,run_id);

CREATE TRIGGER runs_budget_insert_guard
BEFORE INSERT ON runs
WHEN NEW.max_steps > 64 OR NEW.max_tool_calls > 256 OR
     NEW.max_input_tokens > 1000000000 OR NEW.max_output_tokens > 1000000000 OR
     NEW.max_total_tokens > 1000000000 OR NEW.max_output_bytes > 65536 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.provider_timeout_ms < 100 OR NEW.provider_timeout_ms > 120000 OR
     NEW.used_total_tokens != NEW.used_input_tokens + NEW.used_output_tokens OR
     NEW.used_input_tokens > NEW.max_input_tokens OR
     NEW.used_output_tokens > NEW.max_output_tokens OR
     NEW.used_total_tokens > NEW.max_total_tokens OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT,'invalid run budget');
END;

CREATE TRIGGER runs_budget_update_guard
BEFORE UPDATE OF max_steps,max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,
                 max_output_bytes,max_cost_microusd,provider_timeout_ms,
                 used_input_tokens,used_output_tokens,used_total_tokens,used_cost_microusd ON runs
WHEN NEW.max_steps > 64 OR NEW.max_tool_calls > 256 OR
     NEW.max_input_tokens > 1000000000 OR NEW.max_output_tokens > 1000000000 OR
     NEW.max_total_tokens > 1000000000 OR NEW.max_output_bytes > 65536 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.provider_timeout_ms < 100 OR NEW.provider_timeout_ms > 120000 OR
     NEW.used_total_tokens != NEW.used_input_tokens + NEW.used_output_tokens OR
     NEW.used_input_tokens > NEW.max_input_tokens OR
     NEW.used_output_tokens > NEW.max_output_tokens OR
     NEW.used_total_tokens > NEW.max_total_tokens OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT,'invalid run budget');
END;

CREATE TABLE capability_audit_v14 (
  app_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  capability_id TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload)),
  success INTEGER NOT NULL CHECK(success IN (0,1)),
  error_code TEXT,
  error_message TEXT,
  duration_ms INTEGER NOT NULL CHECK(duration_ms>=0),
  created_at TEXT NOT NULL,
  PRIMARY KEY(app_id,run_id,call_id),
  FOREIGN KEY(app_id,echo_id) REFERENCES echoes(app_id,echo_id) ON DELETE CASCADE,
  FOREIGN KEY(app_id,run_id) REFERENCES runs(app_id,run_id) ON DELETE CASCADE
);
INSERT INTO capability_audit_v14(
  app_id,run_id,call_id,echo_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at
)
SELECT
  audit.app_id,
  (SELECT run_id FROM runs
   WHERE app_id=audit.app_id AND echo_id=audit.echo_id
   ORDER BY created_at,run_id LIMIT 1),
  audit.call_id,audit.echo_id,audit.capability_id,audit.payload,audit.success,
  audit.error_code,audit.error_message,audit.duration_ms,audit.created_at
FROM capability_audit audit;
DROP TABLE capability_audit;
ALTER TABLE capability_audit_v14 RENAME TO capability_audit;
CREATE INDEX capability_audit_echo_idx ON capability_audit(app_id,echo_id,created_at,run_id,call_id);
`}
	for index, statements := range baseMigrations {
		registerMigration(index+1, statements)
	}
}

func (s *Store) migrateThrough(ctx context.Context, maximumVersion int) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
		return fmt.Errorf("initialize sqlite migrations: %w", err)
	}
	// WAL 是持久化文件数据库的既定日志模式：只读/不支持 WAL 的文件系统会让
	// PRAGMA 静默退回回滚日志模式，必须校验实际生效模式并 fail-closed，不得
	// 在错误的持久性档案下继续运行。内存数据库只能为 memory 模式，无持久性
	// 要求，允许通过。
	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read sqlite journal mode: %w", err)
	}
	switch strings.ToLower(journalMode) {
	case "wal", "memory":
	default:
		return fmt.Errorf("sqlite journal mode is %q, expected wal (WAL 不可用时拒绝启动，避免静默降级)", journalMode)
	}
	versions := make([]int, 0, len(registeredMigrations))
	for version := range registeredMigrations {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	if len(versions) == 0 {
		return fmt.Errorf("no database migrations are registered")
	}
	if maximumVersion < 0 || (maximumVersion > 0 && versions[len(versions)-1] < maximumVersion) {
		return fmt.Errorf("invalid maximum migration version")
	}
	for _, version := range versions {
		if maximumVersion > 0 && version > maximumVersion {
			continue
		}
		migration := registeredMigrations[version]
		var applied int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if applied > 0 {
			continue
		}
		if err := s.applyMigration(ctx, version, migration); err != nil {
			return err
		}
		observe.Info(ctx, "数据库迁移已经应用",
			observe.Component("storage"),
			observe.IntAttr("migration_version", version),
		)
	}
	return nil
}

// applyMigration 在独立事务中应用单个迁移；事务互斥由 finishTx 在返回时释放
// （迁移可能逐条应用多个版本，互斥必须按事务而非按函数持有）。
func (s *Store) applyMigration(ctx context.Context, version int, migration string) error {
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", version, err)
	}
	var resultErr error
	defer s.finishTx(tx, &resultErr, "apply migration")
	if _, err := tx.ExecContext(ctx, migration); err != nil {
		return errors.Join(fmt.Errorf("apply migration %d: %w", version, err), tx.Rollback())
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return errors.Join(fmt.Errorf("record migration %d: %w", version, err), tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", version, err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
