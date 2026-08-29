package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
)

var _ qqsettings.Store = (*Store)(nil)

func init() {
	registerMigration(22, `
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
`)
}

func (s *Store) EnsureQQSettings(ctx context.Context, seed qqsettings.Settings) (_ qqsettings.Settings, created bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "qq_settings_ensure", started, resultErr) }()
	normalized, err := qqsettings.Normalize(seed)
	if err != nil {
		return qqsettings.Settings{}, false, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return qqsettings.Settings{}, false, fmt.Errorf("begin qq settings ensure: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "ensure qq settings")
	existing, err := readQQSettings(ctx, tx, normalized.AppID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return qqsettings.Settings{}, false, fmt.Errorf("commit qq settings read: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, qqsettings.ErrNotFound) {
		return qqsettings.Settings{}, false, err
	}
	normalized.Generation = 1
	normalized.UpdatedAt = time.Now().UTC()
	groups, privateUsers, err := marshalQQAllowlists(normalized)
	if err != nil {
		return qqsettings.Settings{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO qq_access_settings(
  app_id,enabled,ws_url,bot_qq_id,allowed_group_ids,allowed_private_user_ids,generation,updated_at
) VALUES(?,?,?,?,?,?,?,?)`, normalized.AppID, normalized.Enabled, normalized.WSURL, normalized.BotQQID,
		groups, privateUsers, normalized.Generation, normalized.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return qqsettings.Settings{}, false, fmt.Errorf("insert qq settings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return qqsettings.Settings{}, false, fmt.Errorf("commit qq settings ensure: %w", err)
	}
	return normalized, true, nil
}

func (s *Store) CurrentQQSettings(ctx context.Context, appID string) (_ qqsettings.Settings, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "qq_settings_current", started, resultErr) }()
	return readQQSettings(ctx, s.db, appID)
}

func (s *Store) CompareAndSwapQQSettings(ctx context.Context, expectedGeneration uint64, replacement qqsettings.Settings) (_ qqsettings.Settings, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "qq_settings_update", started, resultErr) }()
	if expectedGeneration == 0 {
		return qqsettings.Settings{}, qqsettings.ErrConflict
	}
	normalized, err := qqsettings.Normalize(replacement)
	if err != nil {
		return qqsettings.Settings{}, err
	}
	current, err := s.CurrentQQSettings(ctx, normalized.AppID)
	if err != nil {
		return qqsettings.Settings{}, err
	}
	if current.Generation != expectedGeneration {
		return qqsettings.Settings{}, qqsettings.ErrConflict
	}
	if qqsettings.EqualContent(current, normalized) {
		return current, nil
	}
	normalized.Generation = current.Generation + 1
	normalized.UpdatedAt = time.Now().UTC()
	groups, privateUsers, err := marshalQQAllowlists(normalized)
	if err != nil {
		return qqsettings.Settings{}, err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE qq_access_settings
SET enabled=?,ws_url=?,bot_qq_id=?,allowed_group_ids=?,allowed_private_user_ids=?,generation=?,updated_at=?
WHERE app_id=? AND generation=?`, normalized.Enabled, normalized.WSURL, normalized.BotQQID, groups, privateUsers,
		normalized.Generation, normalized.UpdatedAt.Format(time.RFC3339Nano), normalized.AppID, expectedGeneration)
	if err != nil {
		return qqsettings.Settings{}, fmt.Errorf("update qq settings: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return qqsettings.Settings{}, fmt.Errorf("read qq settings update count: %w", err)
	}
	if affected != 1 {
		return qqsettings.Settings{}, qqsettings.ErrConflict
	}
	return normalized, nil
}

func readQQSettings(ctx context.Context, queryer rowQueryer, appID string) (qqsettings.Settings, error) {
	seed, err := qqsettings.Normalize(qqsettings.Settings{AppID: appID})
	if err != nil {
		return qqsettings.Settings{}, err
	}
	var enabled bool
	var groupsJSON, privateUsersJSON, updatedAt string
	value := qqsettings.Settings{AppID: seed.AppID}
	err = queryer.QueryRowContext(ctx, `
SELECT enabled,ws_url,bot_qq_id,allowed_group_ids,allowed_private_user_ids,generation,updated_at
FROM qq_access_settings WHERE app_id=?`, seed.AppID).Scan(
		&enabled, &value.WSURL, &value.BotQQID, &groupsJSON, &privateUsersJSON, &value.Generation, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return qqsettings.Settings{}, qqsettings.ErrNotFound
	}
	if err != nil {
		return qqsettings.Settings{}, fmt.Errorf("read qq settings: %w", err)
	}
	value.Enabled = enabled
	if err := json.Unmarshal([]byte(groupsJSON), &value.AllowedGroupIDs); err != nil {
		return qqsettings.Settings{}, errors.Join(qqsettings.ErrInvalid, err)
	}
	if err := json.Unmarshal([]byte(privateUsersJSON), &value.AllowedPrivateUserIDs); err != nil {
		return qqsettings.Settings{}, errors.Join(qqsettings.ErrInvalid, err)
	}
	value.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil || value.Generation == 0 {
		return qqsettings.Settings{}, qqsettings.ErrInvalid
	}
	normalized, err := qqsettings.Normalize(value)
	if err != nil || !qqsettings.EqualContent(normalized, value) || !slices.Equal(normalized.AllowedGroupIDs, value.AllowedGroupIDs) || !slices.Equal(normalized.AllowedPrivateUserIDs, value.AllowedPrivateUserIDs) {
		return qqsettings.Settings{}, qqsettings.ErrInvalid
	}
	return normalized, nil
}

func marshalQQAllowlists(value qqsettings.Settings) (string, string, error) {
	groups, err := json.Marshal(value.AllowedGroupIDs)
	if err != nil {
		return "", "", errors.Join(qqsettings.ErrInvalid, err)
	}
	privateUsers, err := json.Marshal(value.AllowedPrivateUserIDs)
	if err != nil {
		return "", "", errors.Join(qqsettings.ErrInvalid, err)
	}
	return string(groups), string(privateUsers), nil
}
