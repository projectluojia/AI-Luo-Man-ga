package sqlite

// 迁移 27：确认记录只绑定 Capability。旧表中的工具目标没有等价的新语义，
// 因此迁移在发现这类记录时直接失败，不丢弃也不猜测其含义。
func init() {
	registerMigration(27, `
CREATE TABLE confirmations_v27 (
  app_id TEXT NOT NULL,
  confirmation_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  capability_id TEXT NOT NULL,
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
CREATE TEMP TRIGGER confirmations_v27_reject_legacy_targets
BEFORE INSERT ON confirmations_v27
WHEN EXISTS (SELECT 1 FROM confirmations WHERE target_type <> 'capability')
BEGIN
  SELECT RAISE(ABORT, 'legacy confirmation target cannot be migrated');
END;
INSERT INTO confirmations_v27(
  app_id,confirmation_id,echo_id,run_id,call_id,capability_id,side_effect,
  idempotency_key,argument_digest,status,expires_at,confirmed_by,decided_at,created_at
)
SELECT app_id,confirmation_id,echo_id,run_id,call_id,
       CASE WHEN capability_id='' THEN target_id ELSE capability_id END,
       side_effect,idempotency_key,argument_digest,status,expires_at,confirmed_by,decided_at,created_at
FROM confirmations;
DROP TRIGGER confirmations_v27_reject_legacy_targets;
DROP TABLE confirmations;
ALTER TABLE confirmations_v27 RENAME TO confirmations;
CREATE INDEX confirmations_run_idx ON confirmations(app_id, run_id);
CREATE INDEX confirmations_status_expiry_idx ON confirmations(app_id, status, expires_at);
`)
}
