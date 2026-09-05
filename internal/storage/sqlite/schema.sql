CREATE TABLE users (
  user_id TEXT PRIMARY KEY CHECK(length(user_id) BETWEEN 1 AND 128),
  status TEXT NOT NULL CHECK(status IN ('active','disabled')),
  created_at TEXT NOT NULL,
  disabled_at TEXT,
  CHECK((status='active' AND disabled_at IS NULL) OR (status='disabled' AND disabled_at IS NOT NULL))
);

CREATE TABLE roles (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  role_id TEXT NOT NULL CHECK(length(role_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  description TEXT NOT NULL CHECK(length(description) <= 1024),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, role_id)
);

CREATE TABLE app_memberships (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  role_ids TEXT NOT NULL CHECK(length(role_ids) <= 8192 AND json_valid(role_ids) AND json_type(role_ids)='array'),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, user_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);

CREATE TABLE permission_grants (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT CHECK(length(user_id) BETWEEN 1 AND 128),
  role_id TEXT CHECK(length(role_id) BETWEEN 1 AND 128),
  permission TEXT NOT NULL CHECK(length(permission) BETWEEN 1 AND 128),
  granted_at TEXT NOT NULL,
  CHECK((user_id IS NULL) <> (role_id IS NULL)),
  FOREIGN KEY (app_id, user_id) REFERENCES app_memberships(app_id, user_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, role_id) REFERENCES roles(app_id, role_id) ON DELETE CASCADE
);

CREATE TABLE identity_binding_revisions (
  app_id TEXT PRIMARY KEY CHECK(length(app_id) BETWEEN 1 AND 128),
  revision INTEGER NOT NULL CHECK(revision > 0),
  updated_at TEXT NOT NULL
);

CREATE TABLE external_identities (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  platform TEXT NOT NULL CHECK(length(platform) BETWEEN 1 AND 128),
  platform_space_id TEXT NOT NULL CHECK(length(platform_space_id) BETWEEN 1 AND 128),
  platform_user_id TEXT NOT NULL CHECK(length(platform_user_id) BETWEEN 1 AND 256),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  bound_at TEXT NOT NULL,
  PRIMARY KEY (app_id, platform, platform_space_id, platform_user_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);

CREATE TABLE sessions (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  type TEXT NOT NULL CHECK(type IN ('direct','group','system')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL CHECK(julianday(updated_at) >= julianday(created_at)),
  PRIMARY KEY (app_id, session_id)
);

CREATE TABLE session_members (
  app_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  role TEXT NOT NULL CHECK(role IN ('owner','admin','member')),
  joined_at TEXT NOT NULL,
  PRIMARY KEY (app_id, session_id, user_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE
);

CREATE TABLE session_bindings (
  app_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  platform TEXT NOT NULL CHECK(length(platform) BETWEEN 1 AND 64),
  platform_id TEXT NOT NULL CHECK(length(platform_id) BETWEEN 1 AND 256),
  bound_at TEXT NOT NULL,
  PRIMARY KEY (app_id, session_id, platform, platform_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE
);

CREATE TABLE messages (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  sender_user_id TEXT NOT NULL CHECK(length(sender_user_id) BETWEEN 1 AND 128),
  type TEXT NOT NULL CHECK(type IN ('text','image','file','system','event')),
  content_mode TEXT NOT NULL CHECK(content_mode IN ('inline','blob')),
  content_blob_id TEXT NOT NULL DEFAULT '',
  content_size INTEGER NOT NULL CHECK(content_size > 0),
  content BLOB NOT NULL,
  reply_to TEXT NOT NULL DEFAULT '',
  platform_message_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  edited_at TEXT,
  deleted_at TEXT,
  PRIMARY KEY (app_id, message_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE,
  CHECK(
    (content_mode='inline' AND content_blob_id='' AND content_size <= 262144 AND length(content) BETWEEN 1 AND 262144 AND content_size = length(content)) OR
    (content_mode='blob' AND content_blob_id <> '' AND content_size <= 16777216 AND length(content) = 0)
  )
);

CREATE TABLE attachments (
  app_id TEXT NOT NULL,
  attachment_id TEXT NOT NULL CHECK(length(attachment_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
  uploader_user_id TEXT NOT NULL CHECK(length(uploader_user_id) BETWEEN 1 AND 128),
  filename TEXT NOT NULL CHECK(length(filename) BETWEEN 1 AND 256),
  mime_type TEXT NOT NULL CHECK(length(mime_type) BETWEEN 1 AND 128),
  size INTEGER NOT NULL CHECK(size > 0 AND size <= 16777216),
  blob_id TEXT NOT NULL CHECK(length(blob_id) BETWEEN 1 AND 256),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, attachment_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, message_id) REFERENCES messages(app_id, message_id) ON DELETE CASCADE
);

CREATE TABLE echoes (
  app_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  input_message TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('running', 'succeeded', 'failed', 'cancelled')),
  final_message TEXT,
  error_code TEXT,
  error_message TEXT,
  created_at TEXT NOT NULL,
  completed_at TEXT,
  next_event_sequence INTEGER NOT NULL DEFAULT 1 CHECK(next_event_sequence > 0),
  result_payload BLOB NOT NULL DEFAULT '' CHECK(length(result_payload)<=262144),
  result_content_type TEXT NOT NULL DEFAULT '' CHECK(length(result_content_type)<=128),
  PRIMARY KEY (app_id, echo_id)
);

CREATE TABLE app_config_revisions (
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

CREATE TABLE app_config_heads (
  app_id TEXT NOT NULL PRIMARY KEY,
  revision TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK(generation > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(app_id,revision) REFERENCES app_config_revisions(app_id,revision) ON DELETE RESTRICT
);

CREATE TABLE runs (
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
  FOREIGN KEY(app_id,parent_run_id) REFERENCES runs(app_id,run_id),
  CHECK(parent_run_id IS NULL OR parent_run_id<>run_id),
  CHECK((parent_run_id IS NULL AND origin_call_id='') OR (parent_run_id IS NOT NULL AND origin_call_id<>''))
);

CREATE TABLE capability_audit (
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

CREATE TABLE echo_events (
  app_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  sequence INTEGER NOT NULL CHECK(sequence > 0),
  type TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload)),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, echo_id, sequence),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes(app_id, echo_id) ON DELETE CASCADE
);

CREATE TABLE echo_create_requests (
  app_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_fingerprint TEXT NOT NULL CHECK(length(request_fingerprint)=64),
  echo_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, idempotency_key),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes(app_id, echo_id) ON DELETE RESTRICT
);

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

CREATE TABLE confirmations (
  app_id TEXT NOT NULL,
  confirmation_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  capability_id TEXT NOT NULL DEFAULT '',
  side_effect TEXT NOT NULL CHECK(side_effect IN ('write','external')),
  idempotency_key TEXT NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
  argument_digest TEXT NOT NULL CHECK(length(argument_digest)=64),
  status TEXT NOT NULL CHECK(status IN ('waiting','approved','rejected','expired','revoked')),
  expires_at TEXT NOT NULL,
  confirmed_by TEXT NOT NULL DEFAULT '',
  decided_at TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, confirmation_id),
  CHECK(julianday(expires_at) > julianday(created_at)),
  CHECK((status='waiting' AND decided_at IS NULL) OR (status<>'waiting' AND decided_at IS NOT NULL)),
  FOREIGN KEY (app_id, echo_id) REFERENCES echoes(app_id, echo_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, run_id) REFERENCES runs(app_id, run_id) ON DELETE CASCADE
);

CREATE TABLE tasks (
  app_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  task_type TEXT NOT NULL CHECK(length(task_type) BETWEEN 1 AND 128),
  status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','retry_scheduled','cancelled')),
  attempt INTEGER NOT NULL CHECK(attempt > 0),
  max_attempts INTEGER NOT NULL CHECK(max_attempts BETWEEN 1 AND 32),
  deadline_at TEXT NOT NULL,
  available_at TEXT NOT NULL,
  lease_token TEXT,
  lease_expires_at TEXT,
  idempotency_key TEXT NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128),
  params TEXT NOT NULL CHECK(json_valid(params)),
  error_class TEXT NOT NULL CHECK(error_class IN ('','retryable','non_retryable','deadline_exceeded','lease_lost','cancelled')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, task_id),
  CHECK(attempt <= max_attempts),
  CHECK((status = 'running') = (lease_token IS NOT NULL)),
  CHECK(lease_token IS NULL OR lease_expires_at IS NOT NULL),
  CHECK(julianday(deadline_at) > julianday(available_at))
);

CREATE TABLE qq_access_settings (
  app_id TEXT PRIMARY KEY CHECK(length(app_id) BETWEEN 1 AND 128),
  enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
  ws_url TEXT NOT NULL CHECK(length(ws_url) <= 2048),
  bot_qq_id TEXT NOT NULL CHECK(length(bot_qq_id) <= 32),
  allowed_group_ids TEXT NOT NULL CHECK(length(allowed_group_ids) <= 32768 AND json_valid(allowed_group_ids) AND json_type(allowed_group_ids)='array'),
  allowed_private_user_ids TEXT NOT NULL CHECK(length(allowed_private_user_ids) <= 32768 AND json_valid(allowed_private_user_ids) AND json_type(allowed_private_user_ids)='array'),
  generation INTEGER NOT NULL CHECK(generation > 0),
  updated_at TEXT NOT NULL
);

CREATE TABLE package_documents (
  app_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  collection TEXT NOT NULL,
  doc_id TEXT NOT NULL,
  payload TEXT NOT NULL CHECK(json_valid(payload) AND length(payload) <= 65536),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, namespace, collection, doc_id)
);

CREATE TABLE package_snapshots (
  app_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 256),
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0,1)),
  complete INTEGER NOT NULL CHECK(complete IN (0,1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  is_current INTEGER NOT NULL CHECK(is_current IN (0,1)),
  PRIMARY KEY (app_id, namespace, revision)
);

CREATE INDEX external_identities_user_idx ON external_identities(app_id, user_id);
CREATE INDEX app_memberships_user_idx ON app_memberships(user_id);
CREATE UNIQUE INDEX permission_grants_user_idx ON permission_grants(app_id, user_id, permission) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX permission_grants_role_idx ON permission_grants(app_id, role_id, permission) WHERE role_id IS NOT NULL;
CREATE INDEX messages_history_idx ON messages(app_id, session_id, created_at, message_id);
CREATE INDEX messages_sender_idx ON messages(app_id, session_id, sender_user_id, created_at);
CREATE UNIQUE INDEX messages_platform_dedup_idx ON messages(app_id, platform_message_id) WHERE platform_message_id <> '';
CREATE INDEX app_config_revision_created_idx ON app_config_revisions(app_id,created_at,revision);
CREATE INDEX runs_queue_idx ON runs(app_id,status,created_at);
CREATE INDEX runs_echo_idx ON runs(app_id,echo_id,run_group_id,attempt);
CREATE INDEX runs_lease_idx ON runs(app_id,status,lease_expires_at);
CREATE INDEX runs_runnable_idx ON runs(app_id,status,available_at,created_at,run_id);
CREATE INDEX runs_parent_idx ON runs(app_id,parent_run_id,created_at,run_id);
CREATE INDEX echo_events_replay_idx ON echo_events(app_id, echo_id, sequence);
CREATE UNIQUE INDEX echo_create_request_echo_idx ON echo_create_requests(app_id, echo_id);
CREATE INDEX idempotency_expiry_idx ON idempotency_records(app_id, expires_at);
CREATE INDEX capability_audit_echo_idx ON capability_audit(app_id,echo_id,created_at,run_id,call_id);
CREATE INDEX confirmations_run_idx ON confirmations(app_id, run_id);
CREATE INDEX confirmations_status_expiry_idx ON confirmations(app_id, status, expires_at);
CREATE INDEX tasks_queue_idx ON tasks(app_id, status, available_at);
CREATE INDEX tasks_lease_idx ON tasks(status, lease_expires_at);
CREATE INDEX tasks_app_lease_idx ON tasks(app_id, status, lease_expires_at);
CREATE INDEX package_documents_scope_idx ON package_documents(app_id, namespace, collection, doc_id);
CREATE UNIQUE INDEX package_snapshots_current_idx ON package_snapshots(app_id, namespace) WHERE is_current = 1;

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
