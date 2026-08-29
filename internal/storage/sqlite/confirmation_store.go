package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
)

// 迁移 17：确认与副作用治理表。所有读写按 (app_id, confirmation_id) 作用域执行，
// 并通过外键把确认绑定到已存在的 Echo 与 Run。
func init() {
	registerMigration(17, `
CREATE TABLE confirmations (
  app_id TEXT NOT NULL,
  confirmation_id TEXT NOT NULL,
  echo_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  capability_id TEXT NOT NULL DEFAULT '',
  target_type TEXT NOT NULL CHECK(target_type IN ('capability','tool')),
  target_id TEXT NOT NULL,
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
CREATE INDEX confirmations_run_idx ON confirmations(app_id, run_id);
CREATE INDEX confirmations_status_expiry_idx ON confirmations(app_id, status, expires_at);
`)
}

func init() {
	registerMigration(25, `
CREATE INDEX IF NOT EXISTS confirmations_echo_idx ON confirmations(app_id, echo_id, status, expires_at);
`)
	// 迁移 26：公共确认往返协议。确认记录补充会话归属（接入层 Intake 权威
	// 生成），支持决策后在同一会话的新 Echo 中携带确认重试；存量记录会话为空，
	// 只能在本 Echo 内重试，行为与迁移前一致。
	registerMigration(26, `
ALTER TABLE confirmations ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS confirmations_session_idx ON confirmations(app_id, session_id, status, expires_at);
`)
}

func (s *Store) ListActiveByEcho(ctx context.Context, appID, echoID string, now time.Time) (_ []confirmation.Confirmation, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_active_confirmations", started, resultErr) }()
	if appID == "" || echoID == "" || now.IsZero() {
		return nil, confirmation.ErrInvalidRequest
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+confirmationColumns+`
FROM confirmations
WHERE app_id=? AND echo_id=? AND status IN (?,?) AND julianday(expires_at)>julianday(?)
ORDER BY created_at ASC, confirmation_id ASC`,
		appID, echoID, confirmation.StatusWaiting, confirmation.StatusApproved, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("list active confirmations: %w", err)
	}
	defer rows.Close()
	records := []confirmation.Confirmation{}
	for rows.Next() {
		record, scanErr := scanConfirmation(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active confirmations: %w", err)
	}
	return records, nil
}

func (s *Store) ListActiveBySession(ctx context.Context, appID, sessionID string, now time.Time) (_ []confirmation.Confirmation, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_session_confirmations", started, resultErr) }()
	if appID == "" || sessionID == "" || now.IsZero() {
		return nil, confirmation.ErrInvalidRequest
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+confirmationColumns+`
FROM confirmations
WHERE app_id=? AND session_id=? AND session_id<>'' AND status IN (?,?) AND julianday(expires_at)>julianday(?)
ORDER BY created_at ASC, confirmation_id ASC`,
		appID, sessionID, confirmation.StatusWaiting, confirmation.StatusApproved, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("list session confirmations: %w", err)
	}
	defer rows.Close()
	records := []confirmation.Confirmation{}
	for rows.Next() {
		record, scanErr := scanConfirmation(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active confirmations: %w", err)
	}
	return records, nil
}

func (s *Store) RevokeEcho(ctx context.Context, appID, echoID string, now time.Time) (_ int64, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "revoke_echo_confirmations", started, resultErr) }()
	if appID == "" || echoID == "" || now.IsZero() {
		return 0, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,decided_at=?
WHERE app_id=? AND echo_id=? AND status IN (?,?)`,
		confirmation.StatusRevoked, now.UTC().Format(time.RFC3339Nano),
		appID, echoID, confirmation.StatusWaiting, confirmation.StatusApproved,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke echo confirmations: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked echo confirmation count: %w", err)
	}
	return affected, nil
}

// confirmationColumns 是确认查询的统一列清单，供单行与列表查询复用。
const confirmationColumns = `app_id,confirmation_id,echo_id,coalesce(session_id,''),run_id,call_id,
coalesce(capability_id,''),target_type,target_id,
side_effect,idempotency_key,argument_digest,status,expires_at,coalesce(confirmed_by,''),decided_at,created_at`

// scanConfirmation 把一行确认查询结果映射为记录；scan 由调用方提供
// （单行 QueryRow 与多行 rows 复用同一映射）。
func scanConfirmation(scan func(dest ...any) error) (confirmation.Confirmation, error) {
	var record confirmation.Confirmation
	var expiresAt, createdAt string
	var decidedAt sql.NullString
	err := scan(
		&record.AppID, &record.ConfirmationID, &record.EchoID, &record.SessionID, &record.RunID, &record.CallID,
		&record.CapabilityID, &record.TargetType, &record.TargetID, &record.SideEffect,
		&record.IdempotencyKey, &record.ArgumentDigest, &record.Status,
		&expiresAt, &record.ConfirmedBy, &decidedAt, &createdAt,
	)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	if record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return confirmation.Confirmation{}, fmt.Errorf("parse confirmation expiry: %w", err)
	}
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return confirmation.Confirmation{}, fmt.Errorf("parse confirmation creation time: %w", err)
	}
	if record.DecidedAt, err = parseOptionalTime(decidedAt); err != nil {
		return confirmation.Confirmation{}, fmt.Errorf("parse confirmation decision time: %w", err)
	}
	return record, nil
}

func (s *Store) Create(ctx context.Context, record confirmation.Confirmation) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_confirmation", started, resultErr) }()
	if err := confirmation.ValidateConfirmation(record); err != nil {
		return err
	}
	if record.Status != confirmation.StatusWaiting || record.DecidedAt != nil || record.ConfirmedBy != "" {
		return fmt.Errorf("%w: new confirmation must be waiting", confirmation.ErrInvalidRequest)
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO confirmations(
  app_id,confirmation_id,echo_id,session_id,run_id,call_id,capability_id,target_type,target_id,
  side_effect,idempotency_key,argument_digest,status,expires_at,created_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.AppID, record.ConfirmationID, record.EchoID, record.SessionID, record.RunID, record.CallID,
		record.CapabilityID, record.TargetType, record.TargetID, record.SideEffect,
		record.IdempotencyKey, record.ArgumentDigest, record.Status,
		record.ExpiresAt.UTC().Format(time.RFC3339Nano), record.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return confirmation.ErrDuplicate
		}
		return fmt.Errorf("create confirmation: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read confirmation insert count: %w", err)
	} else if affected != 1 {
		return fmt.Errorf("create confirmation: unexpected affected rows %d", affected)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, appID, confirmationID string) (_ confirmation.Confirmation, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_confirmation", started, resultErr) }()
	if appID == "" || confirmationID == "" {
		return confirmation.Confirmation{}, confirmation.ErrInvalidRequest
	}
	record, err := scanConfirmation(s.db.QueryRowContext(ctx, `
SELECT `+confirmationColumns+`
FROM confirmations
WHERE app_id=? AND confirmation_id=?`,
		appID, confirmationID,
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return confirmation.Confirmation{}, confirmation.ErrNotFound
	}
	if err != nil {
		return confirmation.Confirmation{}, fmt.Errorf("get confirmation: %w", err)
	}
	return record, nil
}

func (s *Store) Decide(
	ctx context.Context,
	appID, confirmationID, status, confirmedBy string,
	decidedAt time.Time,
) (_ confirmation.Confirmation, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "decide_confirmation", started, resultErr) }()
	if appID == "" || confirmationID == "" ||
		(status != confirmation.StatusApproved && status != confirmation.StatusRejected) ||
		confirmedBy == "" || decidedAt.IsZero() {
		return confirmation.Confirmation{}, false, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,confirmed_by=?,decided_at=?
WHERE app_id=? AND confirmation_id=? AND status=? AND julianday(expires_at)>=julianday(?)`,
		status, confirmedBy, decidedAt.UTC().Format(time.RFC3339Nano),
		appID, confirmationID, confirmation.StatusWaiting, decidedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("decide confirmation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("read confirmation decision count: %w", err)
	}
	record, err := s.Get(ctx, appID, confirmationID)
	if err != nil {
		return confirmation.Confirmation{}, false, err
	}
	return record, affected == 1, nil
}

func (s *Store) Revoke(ctx context.Context, appID, confirmationID string, revokedAt time.Time) (_ confirmation.Confirmation, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "revoke_confirmation", started, resultErr) }()
	if appID == "" || confirmationID == "" || revokedAt.IsZero() {
		return confirmation.Confirmation{}, false, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,decided_at=?
WHERE app_id=? AND confirmation_id=? AND status IN (?,?) AND julianday(expires_at)>=julianday(?)`,
		confirmation.StatusRevoked, revokedAt.UTC().Format(time.RFC3339Nano),
		appID, confirmationID, confirmation.StatusWaiting, confirmation.StatusApproved,
		revokedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("revoke confirmation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("read confirmation revocation count: %w", err)
	}
	record, err := s.Get(ctx, appID, confirmationID)
	if err != nil {
		return confirmation.Confirmation{}, false, err
	}
	return record, affected == 1, nil
}

func (s *Store) Expire(ctx context.Context, appID, confirmationID string, expiredAt time.Time) (_ confirmation.Confirmation, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "expire_confirmation", started, resultErr) }()
	if appID == "" || confirmationID == "" || expiredAt.IsZero() {
		return confirmation.Confirmation{}, false, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,decided_at=?
WHERE app_id=? AND confirmation_id=? AND status IN (?,?)`,
		confirmation.StatusExpired, expiredAt.UTC().Format(time.RFC3339Nano),
		appID, confirmationID, confirmation.StatusWaiting, confirmation.StatusApproved,
	)
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("expire confirmation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return confirmation.Confirmation{}, false, fmt.Errorf("read confirmation expiry count: %w", err)
	}
	record, err := s.Get(ctx, appID, confirmationID)
	if err != nil {
		return confirmation.Confirmation{}, false, err
	}
	return record, affected == 1, nil
}

func (s *Store) ExpireDue(ctx context.Context, appID string, now time.Time) (_ int64, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "expire_due_confirmations", started, resultErr) }()
	if appID == "" || now.IsZero() {
		return 0, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,decided_at=?
WHERE app_id=? AND status IN (?,?) AND julianday(expires_at)<=julianday(?)`,
		confirmation.StatusExpired, now.UTC().Format(time.RFC3339Nano),
		appID, confirmation.StatusWaiting, confirmation.StatusApproved, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("expire due confirmations: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read expired confirmation count: %w", err)
	}
	return affected, nil
}

func (s *Store) RevokeRun(ctx context.Context, appID, runID string, now time.Time) (_ int64, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "revoke_run_confirmations", started, resultErr) }()
	if appID == "" || runID == "" || now.IsZero() {
		return 0, confirmation.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE confirmations
SET status=?,decided_at=?
WHERE app_id=? AND run_id=? AND status IN (?,?)`,
		confirmation.StatusRevoked, now.UTC().Format(time.RFC3339Nano),
		appID, runID, confirmation.StatusWaiting, confirmation.StatusApproved,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke run confirmations: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read revoked run confirmation count: %w", err)
	}
	return affected, nil
}
