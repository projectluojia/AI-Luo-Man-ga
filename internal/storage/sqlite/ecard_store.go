package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	ecard "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

var _ ecard.Store = (*Store)(nil)

func init() {
	registerMigration(32, `
CREATE TABLE ecard_credentials (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  kind TEXT NOT NULL CHECK(kind IN ('cas_cookie','demo_handle')),
  nonce BLOB NOT NULL CHECK(length(nonce) = 12),
  ciphertext BLOB NOT NULL CHECK(length(ciphertext) BETWEEN 16 AND 8192),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT,
  CHECK(expires_at > created_at),
  CHECK(revoked_at IS NULL OR revoked_at >= created_at),
  FOREIGN KEY(user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX ecard_credentials_active_idx
  ON ecard_credentials(app_id, user_id, kind) WHERE revoked_at IS NULL;
CREATE INDEX ecard_credentials_lookup_idx
  ON ecard_credentials(app_id, user_id, kind, expires_at);
`)
}

func (s *Store) PutECardCredential(ctx context.Context, record ecard.CredentialRecord) (result ecard.CredentialRecord, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "ecard_credential_put", started, resultErr) }()
	if err := validateECardRecord(record); err != nil {
		return ecard.CredentialRecord{}, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return ecard.CredentialRecord{}, fmt.Errorf("begin ecard credential write: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "put ecard credential")
	if err := ctx.Err(); err != nil {
		return ecard.CredentialRecord{}, err
	}
	var userExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE user_id=?`, record.UserID).Scan(&userExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ecard.CredentialRecord{}, ecard.ErrUserRequired
		}
		return ecard.CredentialRecord{}, fmt.Errorf("lookup ecard credential owner: %w", err)
	}
	createdAt := record.CreatedAt.UTC().Format(time.RFC3339Nano)
	expiresAt := record.ExpiresAt.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `
UPDATE ecard_credentials
SET nonce=?, ciphertext=?, created_at=?, expires_at=?
WHERE app_id=? AND user_id=? AND kind=? AND revoked_at IS NULL`,
		record.Nonce, record.Ciphertext, createdAt, expiresAt,
		record.AppID, record.UserID, record.Kind,
	)
	if err != nil {
		return ecard.CredentialRecord{}, fmt.Errorf("replace ecard credential: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ecard.CredentialRecord{}, fmt.Errorf("read ecard credential update count: %w", err)
	}
	if affected == 0 {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ecard_credentials(app_id,user_id,kind,nonce,ciphertext,created_at,expires_at,revoked_at)
VALUES(?,?,?,?,?,?,?,NULL)`,
			record.AppID, record.UserID, record.Kind, record.Nonce, record.Ciphertext, createdAt, expiresAt,
		); err != nil {
			return ecard.CredentialRecord{}, fmt.Errorf("insert ecard credential: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ecard.CredentialRecord{}, fmt.Errorf("commit ecard credential: %w", err)
	}
	record.RevokedAt = nil
	return record, nil
}

func (s *Store) GetActiveECardCredential(ctx context.Context, appID, userID, kind string) (ecard.CredentialRecord, error) {
	if err := validateECardOwner(appID, userID, kind); err != nil {
		return ecard.CredentialRecord{}, err
	}
	var record ecard.CredentialRecord
	var createdAt, expiresAt string
	var revokedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT app_id,user_id,kind,nonce,ciphertext,created_at,expires_at,revoked_at
FROM ecard_credentials
WHERE app_id=? AND user_id=? AND kind=? AND revoked_at IS NULL`,
		appID, userID, kind,
	).Scan(&record.AppID, &record.UserID, &record.Kind, &record.Nonce, &record.Ciphertext, &createdAt, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ecard.CredentialRecord{}, ecard.ErrNotFound
	}
	if err != nil {
		return ecard.CredentialRecord{}, fmt.Errorf("read ecard credential: %w", err)
	}
	if err := parseECardTimes(&record, createdAt, expiresAt, revokedAt); err != nil {
		return ecard.CredentialRecord{}, err
	}
	return record, nil
}

func (s *Store) RevokeECardCredential(ctx context.Context, appID, userID, kind string, at time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "ecard_credential_revoke", started, resultErr) }()
	if err := validateECardOwner(appID, userID, kind); err != nil {
		return err
	}
	if at.IsZero() {
		return ecard.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ecard credential revoke: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "revoke ecard credential")
	if _, err := tx.ExecContext(ctx, `
UPDATE ecard_credentials SET revoked_at=?
WHERE app_id=? AND user_id=? AND kind=? AND revoked_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano), appID, userID, kind,
	); err != nil {
		return fmt.Errorf("revoke ecard credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ecard credential revoke: %w", err)
	}
	return nil
}

func (s *Store) GetECardCredentialMeta(ctx context.Context, appID, userID, kind string) (ecard.CredentialMeta, error) {
	if err := validateECardOwner(appID, userID, kind); err != nil {
		return ecard.CredentialMeta{}, err
	}
	var storedKind, createdAt, expiresAt string
	var revokedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT kind,created_at,expires_at,revoked_at
FROM ecard_credentials
WHERE app_id=? AND user_id=? AND kind=?
ORDER BY CASE WHEN revoked_at IS NULL THEN 0 ELSE 1 END, created_at DESC
LIMIT 1`,
		appID, userID, kind,
	).Scan(&storedKind, &createdAt, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ecard.CredentialMeta{}, nil
	}
	if err != nil {
		return ecard.CredentialMeta{}, fmt.Errorf("read ecard credential status: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return ecard.CredentialMeta{}, fmt.Errorf("parse ecard credential created_at: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return ecard.CredentialMeta{}, fmt.Errorf("parse ecard credential expires_at: %w", err)
	}
	return ecard.CredentialMeta{
		Kind:      storedKind,
		Handle:    storedKind,
		CreatedAt: created,
		ExpiresAt: expires,
		Present:   true,
		Revoked:   revokedAt.Valid,
	}, nil
}

func validateECardRecord(record ecard.CredentialRecord) error {
	if err := validateECardOwner(record.AppID, record.UserID, record.Kind); err != nil {
		return err
	}
	if len(record.Nonce) != ecard.GCMNonceSize ||
		len(record.Ciphertext) < ecard.MinCiphertext || len(record.Ciphertext) > ecard.MaxCiphertext ||
		record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return ecard.ErrInvalid
	}
	return nil
}

func validateECardOwner(appID, userID, kind string) error {
	if err := identity.ValidateAppID(appID); err != nil {
		return ecard.ErrInvalid
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return ecard.ErrUserRequired
	}
	if kind != ecard.KindCASCookie && kind != ecard.KindDemoHandle {
		return ecard.ErrInvalid
	}
	return nil
}

func parseECardTimes(record *ecard.CredentialRecord, createdAt, expiresAt string, revokedAt sql.NullString) error {
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return fmt.Errorf("parse ecard credential created_at: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return fmt.Errorf("parse ecard credential expires_at: %w", err)
	}
	record.CreatedAt = created
	record.ExpiresAt = expires
	if revokedAt.Valid {
		revoked, err := time.Parse(time.RFC3339Nano, revokedAt.String)
		if err != nil {
			return fmt.Errorf("parse ecard credential revoked_at: %w", err)
		}
		record.RevokedAt = &revoked
	}
	return nil
}
