package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
)

func (s *Store) BeginIdempotent(ctx context.Context, claim idempotency.Claim, now time.Time) (_ idempotency.Record, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "begin_idempotency", started, resultErr) }()
	if err := idempotency.ValidateOperation(claim.Operation); err != nil ||
		claim.LeaseToken == "" || now.IsZero() || !claim.LeaseExpiresAt.After(now) {
		return idempotency.Record{}, false, idempotency.ErrInvalidRequest
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO idempotency_records(
  app_id,scope,idempotency_key,request_fingerprint,owner_id,status,
  lease_token,lease_expires_at,created_at
) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(app_id,scope,idempotency_key) DO NOTHING`,
		claim.AppID, claim.Scope, claim.Key, claim.Fingerprint, claim.OwnerID,
		idempotency.StatusExecuting, claim.LeaseToken,
		claim.LeaseExpiresAt.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return idempotency.Record{}, false, fmt.Errorf("begin idempotent operation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return idempotency.Record{}, false, fmt.Errorf("read idempotent insert count: %w", err)
	}
	record, err := s.GetIdempotent(ctx, claim.AppID, claim.Scope, claim.Key)
	if err != nil {
		return idempotency.Record{}, false, err
	}
	if record.Fingerprint != claim.Fingerprint {
		return idempotency.Record{}, false, idempotency.ErrKeyConflict
	}
	return record, affected == 1, nil
}

func (s *Store) GetIdempotent(ctx context.Context, appID, scope, key string) (_ idempotency.Record, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_idempotency", started, resultErr) }()
	if appID == "" || scope == "" || idempotency.ValidateKey(key) != nil {
		return idempotency.Record{}, idempotency.ErrInvalidRequest
	}
	var record idempotency.Record
	var result []byte
	var createdAt string
	var leaseExpiresAt string
	var completedAt sql.NullString
	var expiresAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT app_id,scope,idempotency_key,request_fingerprint,owner_id,status,
       lease_token,lease_expires_at,result,error_code,created_at,completed_at,expires_at
FROM idempotency_records
WHERE app_id=? AND scope=? AND idempotency_key=?`,
		appID, scope, key,
	).Scan(
		&record.AppID, &record.Scope, &record.Key, &record.Fingerprint, &record.OwnerID,
		&record.Status, &record.LeaseToken, &leaseExpiresAt, &result, &record.ErrorCode,
		&createdAt, &completedAt, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return idempotency.Record{}, idempotency.ErrRecordNotFound
	}
	if err != nil {
		return idempotency.Record{}, fmt.Errorf("get idempotent operation: %w", err)
	}
	var parseErr error
	record.LeaseExpiresAt, parseErr = time.Parse(time.RFC3339Nano, leaseExpiresAt)
	if parseErr != nil {
		return idempotency.Record{}, fmt.Errorf("parse idempotency lease expiry: %w", parseErr)
	}
	record.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
	if parseErr != nil {
		return idempotency.Record{}, fmt.Errorf("parse idempotency creation time: %w", parseErr)
	}
	record.CompletedAt, parseErr = parseOptionalTime(completedAt)
	if parseErr != nil {
		return idempotency.Record{}, fmt.Errorf("parse idempotency completion time: %w", parseErr)
	}
	record.ExpiresAt, parseErr = parseOptionalTime(expiresAt)
	if parseErr != nil {
		return idempotency.Record{}, fmt.Errorf("parse idempotency retention expiry: %w", parseErr)
	}
	record.Result = append([]byte(nil), result...)
	return record, nil
}

func (s *Store) CompleteIdempotent(
	ctx context.Context,
	claim idempotency.Claim,
	status string,
	result []byte,
	errorCode string,
	completedAt time.Time,
	expiresAt time.Time,
) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "complete_idempotency", started, resultErr) }()
	if err := idempotency.ValidateOperation(claim.Operation); err != nil ||
		claim.LeaseToken == "" || completedAt.IsZero() || !expiresAt.After(completedAt) ||
		(status != idempotency.StatusSucceeded && status != idempotency.StatusFailed) ||
		(status == idempotency.StatusSucceeded && errorCode != "") ||
		(status == idempotency.StatusFailed && (len(result) != 0 || errorCode != "operation_failed")) {
		return idempotency.ErrInvalidRequest
	}
	var storedResult any
	if status == idempotency.StatusSucceeded {
		storedResult = append([]byte(nil), result...)
	}
	update, err := s.db.ExecContext(ctx, `
UPDATE idempotency_records
SET status=?,result=?,error_code=?,completed_at=?,expires_at=?
WHERE app_id=? AND scope=? AND idempotency_key=?
  AND request_fingerprint=? AND owner_id=? AND status=?
  AND lease_token=? AND julianday(lease_expires_at)>=julianday(?)`,
		status, storedResult, errorCode,
		completedAt.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano),
		claim.AppID, claim.Scope, claim.Key, claim.Fingerprint, claim.OwnerID,
		idempotency.StatusExecuting, claim.LeaseToken, completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("complete idempotent operation: %w", err)
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return fmt.Errorf("read idempotent completion count: %w", err)
	}
	if affected != 1 {
		return idempotency.ErrLeaseLost
	}
	return nil
}
