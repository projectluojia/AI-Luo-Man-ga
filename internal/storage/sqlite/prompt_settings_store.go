package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/providers/prompt"
)

// 编译期断言：sqlite.Store 必须完整实现 prompt.SettingsStore 端口。
var _ promptservice.SettingsStore = (*Store)(nil)

func init() {
	registerMigration(23, `
CREATE TABLE user_prompt_settings (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  basic_style TEXT NOT NULL CHECK(length(basic_style) BETWEEN 1 AND 128),
  extra_trait_levels TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(extra_trait_levels) AND json_type(extra_trait_levels)='object'),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, user_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE INDEX user_prompt_settings_user_idx ON user_prompt_settings(user_id);
`)
}

func (s *Store) GetPromptSettings(ctx context.Context, appID, userID string) (_ promptservice.Settings, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "prompt_settings_get", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return promptservice.Settings{}, err
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return promptservice.Settings{}, err
	}
	var basicStyle, levelsJSON, updatedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT basic_style,extra_trait_levels,updated_at
FROM user_prompt_settings WHERE app_id=? AND user_id=?`, appID, userID,
	).Scan(&basicStyle, &levelsJSON, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return promptservice.Settings{}, promptservice.ErrNotFound
	}
	if err != nil {
		return promptservice.Settings{}, fmt.Errorf("read prompt settings: %w", err)
	}
	var levels map[string]string
	if err := json.Unmarshal([]byte(levelsJSON), &levels); err != nil {
		return promptservice.Settings{}, errors.Join(promptservice.ErrInvalid, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return promptservice.Settings{}, errors.Join(promptservice.ErrInvalid, err)
	}
	settings := promptservice.Settings{
		UserID: userID, BasicStyle: basicStyle, ExtraTraitLevels: levels,
	}
	return promptservice.NormalizeSettings(settings)
}

func (s *Store) SavePromptSettings(ctx context.Context, appID string, settings promptservice.Settings) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "prompt_settings_save", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return err
	}
	if err := identity.ValidateUserID(settings.UserID); err != nil {
		return err
	}
	settings, err := promptservice.NormalizeSettings(settings)
	if err != nil {
		return err
	}
	levelsJSON, err := json.Marshal(settings.ExtraTraitLevels)
	if err != nil {
		return errors.Join(promptservice.ErrInvalid, err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO user_prompt_settings(app_id,user_id,basic_style,extra_trait_levels,updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(app_id,user_id) DO UPDATE SET
  basic_style=excluded.basic_style,
  extra_trait_levels=excluded.extra_trait_levels,
  updated_at=excluded.updated_at`,
		appID, settings.UserID, settings.BasicStyle, string(levelsJSON), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save prompt settings: %w", err)
	}
	return nil
}

func (s *Store) DeletePromptSettings(ctx context.Context, appID, userID string) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "prompt_settings_delete", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return err
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM user_prompt_settings WHERE app_id=? AND user_id=?`, appID, userID)
	if err != nil {
		return fmt.Errorf("delete prompt settings: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read prompt settings delete count: %w", err)
	}
	if affected == 0 {
		return promptservice.ErrNotFound
	}
	return nil
}
