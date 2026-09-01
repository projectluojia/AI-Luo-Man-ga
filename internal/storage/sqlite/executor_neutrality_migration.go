package sqlite

// 迁移 26：把 App 配置和 Run 的执行字段收敛为 Executor 中立契约。
// 旧表中的模型、Provider 和提示词字段只在本次前向迁移中读取；迁移完成后，
// 当前表不再暴露这些字段，ExecutorConfig 作为不透明 JSON 由具体执行者解释。
func init() {
	registerMigration(26, `
DROP TRIGGER IF EXISTS runs_budget_insert_guard;
DROP TRIGGER IF EXISTS runs_budget_update_guard;

ALTER TABLE echoes ADD COLUMN result_payload BLOB NOT NULL DEFAULT '' CHECK(length(result_payload)<=262144);
ALTER TABLE echoes ADD COLUMN result_content_type TEXT NOT NULL DEFAULT '' CHECK(length(result_content_type)<=128);
UPDATE echoes
SET result_payload=CAST(coalesce(final_message,'') AS BLOB),
    result_content_type=CASE WHEN coalesce(final_message,'')='' THEN '' ELSE 'text/plain; charset=utf-8' END;

CREATE TABLE app_config_revisions_v26 (
  app_id TEXT NOT NULL,
  revision TEXT NOT NULL CHECK(length(revision)=64),
  enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
  executor_id TEXT NOT NULL CHECK(length(executor_id) BETWEEN 1 AND 256),
  executor_config TEXT NOT NULL CHECK(length(executor_config) BETWEEN 2 AND 65536 AND json_valid(executor_config)),
  max_steps INTEGER NOT NULL CHECK(max_steps BETWEEN 1 AND 64),
  max_capability_calls INTEGER NOT NULL CHECK(max_capability_calls BETWEEN 1 AND 256),
  max_execution_units INTEGER NOT NULL CHECK(max_execution_units BETWEEN 1 AND 1000000000),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes BETWEEN 1 AND 262144),
  max_cost_microusd INTEGER NOT NULL CHECK(max_cost_microusd BETWEEN 0 AND 1000000000000000),
  execution_timeout_ms INTEGER NOT NULL CHECK(execution_timeout_ms BETWEEN 100 AND 300000),
  enabled_capabilities TEXT NOT NULL CHECK(length(enabled_capabilities)<=65536 AND json_valid(enabled_capabilities) AND json_type(enabled_capabilities)='array'),
  permission_scope TEXT NOT NULL CHECK(length(permission_scope)<=65536 AND json_valid(permission_scope) AND json_type(permission_scope)='array'),
  created_at TEXT NOT NULL,
  PRIMARY KEY(app_id,revision)
);
INSERT INTO app_config_revisions_v26(
  app_id,revision,enabled,executor_id,executor_config,max_steps,max_capability_calls,
  max_execution_units,max_output_bytes,max_cost_microusd,execution_timeout_ms,
  enabled_capabilities,permission_scope,created_at
)
SELECT
  app_id,revision,enabled,model,
  json_object(
    'system_prompt',system_prompt,
    'channel_prompts',json(channel_prompts),
    'timezone',timezone
  ),
  max_steps,max_tool_calls,max_total_tokens,max_output_bytes,max_cost_microusd,
  provider_timeout_ms,enabled_capabilities,permission_scope,created_at
FROM app_config_revisions;

CREATE TABLE app_config_heads_v26 (
  app_id TEXT NOT NULL PRIMARY KEY,
  revision TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK(generation > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(app_id,revision) REFERENCES app_config_revisions_v26(app_id,revision) ON DELETE RESTRICT
);
INSERT INTO app_config_heads_v26(app_id,revision,generation,created_at,updated_at)
SELECT app_id,revision,generation,created_at,updated_at FROM app_config_heads;
DROP TABLE app_config_heads;
DROP TABLE app_config_revisions;
ALTER TABLE app_config_revisions_v26 RENAME TO app_config_revisions;
ALTER TABLE app_config_heads_v26 RENAME TO app_config_heads;
CREATE INDEX app_config_revision_created_idx ON app_config_revisions(app_id,created_at,revision);

CREATE TABLE runs_v26 (
  app_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_group_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  parent_run_id TEXT,
  origin_call_id TEXT NOT NULL DEFAULT '' CHECK(length(origin_call_id)<=128),
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled','timed_out')),
  executor_id TEXT NOT NULL CHECK(length(executor_id) BETWEEN 1 AND 256),
  config_revision TEXT NOT NULL,
  protocol_version TEXT NOT NULL,
  executor_config TEXT NOT NULL CHECK(length(executor_config)>=2 AND length(executor_config)<=65536 AND json_valid(executor_config)),
  input_payload BLOB NOT NULL CHECK(length(input_payload) BETWEEN 1 AND 16384),
  input_content_type TEXT NOT NULL CHECK(length(input_content_type) BETWEEN 1 AND 128),
  max_steps INTEGER NOT NULL CHECK(max_steps BETWEEN 1 AND 64),
  max_capability_calls INTEGER NOT NULL CHECK(max_capability_calls BETWEEN 1 AND 256),
  max_execution_units INTEGER NOT NULL CHECK(max_execution_units BETWEEN 1 AND 1000000000),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes BETWEEN 1 AND 262144),
  max_cost_microusd INTEGER NOT NULL CHECK(max_cost_microusd BETWEEN 0 AND 1000000000000000),
  execution_timeout_ms INTEGER NOT NULL CHECK(execution_timeout_ms BETWEEN 100 AND 300000),
  used_execution_units INTEGER NOT NULL DEFAULT 0 CHECK(used_execution_units>=0),
  used_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK(used_cost_microusd>=0),
  used_retries INTEGER NOT NULL DEFAULT 0 CHECK(used_retries BETWEEN 0 AND 320),
  deadline_at TEXT NOT NULL,
  available_at TEXT NOT NULL,
  lease_token TEXT,
  lease_expires_at TEXT,
  last_executor_sequence INTEGER NOT NULL DEFAULT 0 CHECK(last_executor_sequence>=0),
  capability_scope TEXT NOT NULL DEFAULT '[]' CHECK(length(capability_scope)<=65536 AND json_valid(capability_scope) AND json_type(capability_scope)='array'),
  permission_scope TEXT NOT NULL DEFAULT '[]' CHECK(length(permission_scope)<=65536 AND json_valid(permission_scope) AND json_type(permission_scope)='array'),
  recoverable_state TEXT NOT NULL CHECK(json_valid(recoverable_state)),
  result_payload BLOB NOT NULL DEFAULT '' CHECK(length(result_payload)<=262144),
  result_content_type TEXT NOT NULL DEFAULT '' CHECK(length(result_content_type)<=128),
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  session_id TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  message_id TEXT NOT NULL DEFAULT '',
  context_digest TEXT NOT NULL DEFAULT '',
  context_sources TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(context_sources)),
  channel TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(app_id,run_id),
  UNIQUE(app_id,run_group_id,attempt),
  UNIQUE(app_id,parent_run_id,origin_call_id),
  FOREIGN KEY(app_id,echo_id) REFERENCES echoes(app_id,echo_id) ON DELETE CASCADE,
  FOREIGN KEY(app_id,parent_run_id) REFERENCES runs_v26(app_id,run_id),
  CHECK(parent_run_id IS NULL OR parent_run_id<>run_id),
  CHECK((parent_run_id IS NULL AND origin_call_id='') OR (parent_run_id IS NOT NULL AND origin_call_id<>''))
);
INSERT INTO runs_v26(
  app_id,run_id,run_group_id,echo_id,parent_run_id,origin_call_id,attempt,status,
  executor_id,config_revision,protocol_version,executor_config,input_payload,input_content_type,
  max_steps,max_capability_calls,max_execution_units,max_output_bytes,max_cost_microusd,
  execution_timeout_ms,used_execution_units,used_cost_microusd,used_retries,deadline_at,available_at,
  lease_token,lease_expires_at,last_executor_sequence,capability_scope,permission_scope,
  recoverable_state,result_payload,result_content_type,error_code,error_message,created_at,started_at,
  completed_at,session_id,user_id,message_id,context_digest,context_sources,channel
)
SELECT
  r.app_id,r.run_id, r.run_group_id,r.echo_id,r.parent_run_id,r.origin_call_id,r.attempt,r.status,
  r.model,r.model_config_version,r.protocol_version,
  coalesce((SELECT executor_config FROM app_config_revisions c
            WHERE c.app_id=r.app_id AND c.revision=r.model_config_version),'{}'),
  CAST(CASE WHEN coalesce(r.task_message,'')<>'' THEN r.task_message ELSE e.input_message END AS BLOB),
  'text/plain; charset=utf-8',
  max(1,r.max_steps),max(1,r.max_tool_calls),max(1,r.max_total_tokens),max(1,r.max_output_bytes),r.max_cost_microusd,
  max(100,r.provider_timeout_ms),r.used_total_tokens,r.used_cost_microusd,r.used_provider_retries,
  r.deadline_at,r.available_at,r.lease_token,r.lease_expires_at,r.last_agent_sequence,
  r.capability_scope,r.permission_scope,r.recoverable_state,
  CAST(coalesce(r.result_message,'') AS BLOB),
  CASE WHEN coalesce(r.result_message,'')='' THEN '' ELSE 'text/plain; charset=utf-8' END,
  r.error_code,r.error_message,r.created_at,r.started_at,r.completed_at,
  coalesce(r.session_id,''),coalesce(r.user_id,''),coalesce(r.message_id,''),
  coalesce(r.context_digest,''),coalesce(r.context_sources,'{}'),coalesce(r.channel,'')
FROM runs r
JOIN echoes e ON e.app_id=r.app_id AND e.echo_id=r.echo_id;

CREATE TABLE capability_audit_v26 (
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
  FOREIGN KEY(app_id,run_id) REFERENCES runs_v26(app_id,run_id) ON DELETE CASCADE
);
INSERT INTO capability_audit_v26(
  app_id,run_id,call_id,echo_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at
)
SELECT app_id,run_id,call_id,echo_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at
FROM capability_audit;
DROP TABLE capability_audit;
DROP TABLE runs;
ALTER TABLE runs_v26 RENAME TO runs;
ALTER TABLE capability_audit_v26 RENAME TO capability_audit;
CREATE INDEX runs_queue_idx ON runs(app_id,status,created_at);
CREATE INDEX runs_echo_idx ON runs(app_id,echo_id,run_group_id,attempt);
CREATE INDEX runs_lease_idx ON runs(app_id,status,lease_expires_at);
CREATE INDEX runs_runnable_idx ON runs(app_id,status,available_at,created_at,run_id);
CREATE INDEX runs_parent_idx ON runs(app_id,parent_run_id,created_at,run_id);
CREATE INDEX capability_audit_echo_idx ON capability_audit(app_id,echo_id,created_at,run_id,call_id);

CREATE TRIGGER runs_budget_insert_guard
BEFORE INSERT ON runs
WHEN NEW.max_steps > 64 OR NEW.max_capability_calls > 256 OR
     NEW.max_execution_units > 1000000000 OR NEW.max_output_bytes > 262144 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.used_execution_units > NEW.max_execution_units OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT,'invalid run budget');
END;

CREATE TRIGGER runs_budget_update_guard
BEFORE UPDATE OF max_steps,max_capability_calls,max_execution_units,max_output_bytes,
                 max_cost_microusd,used_execution_units,used_cost_microusd ON runs
WHEN NEW.max_steps > 64 OR NEW.max_capability_calls > 256 OR
     NEW.max_execution_units > 1000000000 OR NEW.max_output_bytes > 262144 OR
     NEW.max_cost_microusd > 1000000000000000 OR
     NEW.used_execution_units > NEW.max_execution_units OR
     (NEW.max_cost_microusd > 0 AND NEW.used_cost_microusd > NEW.max_cost_microusd)
BEGIN
  SELECT RAISE(ABORT,'invalid run budget');
END;
`)
}
