package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

func (s *Store) Ensure(ctx context.Context, seed appconfig.Config) (result appconfig.Config, created bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "app_config_ensure", started, resultErr) }()
	normalized, err := appconfig.Normalize(seed)
	if err != nil {
		return appconfig.Config{}, false, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return appconfig.Config{}, false, fmt.Errorf("begin app config ensure: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "app config ensure")
	existing, err := readCurrentAppConfig(ctx, tx, normalized.AppID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return appconfig.Config{}, false, fmt.Errorf("commit app config read: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, appconfig.ErrNotFound) {
		return appconfig.Config{}, false, err
	}
	now := time.Now().UTC()
	normalized.Generation = 1
	normalized.CreatedAt = now
	normalized.UpdatedAt = now
	if err := insertAppConfigRevision(ctx, tx, normalized); err != nil {
		return appconfig.Config{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO app_config_heads(app_id,revision,generation,created_at,updated_at)
VALUES(?,?,?,?,?)`,
		normalized.AppID, normalized.Revision, normalized.Generation,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return appconfig.Config{}, false, fmt.Errorf("insert app config head: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return appconfig.Config{}, false, fmt.Errorf("commit app config ensure: %w", err)
	}
	return normalized, true, nil
}

func (s *Store) Current(ctx context.Context, appID string) (result appconfig.Config, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "app_config_current", started, resultErr) }()
	if err := appconfig.ValidateAppID(appID); err != nil {
		return appconfig.Config{}, err
	}
	return readCurrentAppConfig(ctx, s.db, appID)
}

func (s *Store) Revision(ctx context.Context, appID, revision string) (result appconfig.Config, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "app_config_revision", started, resultErr) }()
	if err := appconfig.ValidateAppID(appID); err != nil {
		return appconfig.Config{}, err
	}
	if err := appconfig.ValidateRevision(revision); err != nil {
		return appconfig.Config{}, appconfig.ErrInvalid
	}
	row := s.db.QueryRowContext(ctx, `
SELECT r.app_id,r.revision,0,r.enabled,r.executor_id,r.executor_config,
       r.max_steps,r.max_capability_calls,r.max_execution_units,r.max_output_bytes,
       r.max_cost_microusd,r.execution_timeout_ms,r.enabled_capabilities,r.permission_scope,
       r.created_at,r.created_at
FROM app_config_revisions r
WHERE r.app_id=? AND r.revision=?`, appID, revision)
	config, err := scanAppConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return appconfig.Config{}, appconfig.ErrNotFound
	}
	return config, err
}

func (s *Store) CompareAndSwap(ctx context.Context, expectedGeneration uint64, replacement appconfig.Config) (result appconfig.Config, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "app_config_update", started, resultErr) }()
	if expectedGeneration == 0 {
		return appconfig.Config{}, appconfig.ErrConflict
	}
	normalized, err := appconfig.Normalize(replacement)
	if err != nil {
		return appconfig.Config{}, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return appconfig.Config{}, fmt.Errorf("begin app config update: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "app config update")
	current, err := readCurrentAppConfig(ctx, tx, normalized.AppID)
	if err != nil {
		return appconfig.Config{}, err
	}
	if current.Generation != expectedGeneration {
		return appconfig.Config{}, appconfig.ErrConflict
	}
	if current.Revision == normalized.Revision {
		if err := tx.Commit(); err != nil {
			return appconfig.Config{}, fmt.Errorf("commit unchanged app config: %w", err)
		}
		return current, nil
	}
	now := time.Now().UTC()
	normalized.Generation = current.Generation + 1
	normalized.CreatedAt = now
	normalized.UpdatedAt = now
	if err := insertAppConfigRevision(ctx, tx, normalized); err != nil {
		return appconfig.Config{}, err
	}
	resultSQL, err := tx.ExecContext(ctx, `
UPDATE app_config_heads
SET revision=?,generation=?,updated_at=?
WHERE app_id=? AND generation=?`,
		normalized.Revision, normalized.Generation, now.Format(time.RFC3339Nano),
		normalized.AppID, expectedGeneration)
	if err != nil {
		return appconfig.Config{}, fmt.Errorf("update app config head: %w", err)
	}
	affected, err := resultSQL.RowsAffected()
	if err != nil {
		return appconfig.Config{}, fmt.Errorf("read app config update count: %w", err)
	}
	if affected != 1 {
		return appconfig.Config{}, appconfig.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return appconfig.Config{}, fmt.Errorf("commit app config update: %w", err)
	}
	normalized.CreatedAt = current.CreatedAt
	return normalized, nil
}

func readCurrentAppConfig(ctx context.Context, queryer rowQueryer, appID string) (appconfig.Config, error) {
	row := queryer.QueryRowContext(ctx, `
SELECT r.app_id,r.revision,h.generation,r.enabled,r.executor_id,r.executor_config,
       r.max_steps,r.max_capability_calls,r.max_execution_units,r.max_output_bytes,
       r.max_cost_microusd,r.execution_timeout_ms,r.enabled_capabilities,r.permission_scope,
       h.created_at,h.updated_at
FROM app_config_heads h
JOIN app_config_revisions r ON r.app_id=h.app_id AND r.revision=h.revision
WHERE h.app_id=?`, appID)
	config, err := scanAppConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return appconfig.Config{}, appconfig.ErrNotFound
	}
	return config, err
}

func insertAppConfigRevision(ctx context.Context, tx *sql.Tx, config appconfig.Config) error {
	capabilities, err := json.Marshal(config.EnabledCapabilities)
	if err != nil {
		return errors.Join(appconfig.ErrInvalid, err)
	}
	permissions, err := json.Marshal(config.PermissionScope)
	if err != nil {
		return errors.Join(appconfig.ErrInvalid, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO app_config_revisions(
  app_id,revision,enabled,executor_id,executor_config,max_steps,max_capability_calls,
  max_execution_units,max_output_bytes,max_cost_microusd,execution_timeout_ms,
  enabled_capabilities,permission_scope,created_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(app_id,revision) DO NOTHING`,
		config.AppID, config.Revision, config.Enabled, config.ExecutorID, string(config.ExecutorConfig),
		config.MaxSteps, config.MaxCapabilityCalls, config.MaxExecutionUnits, config.MaxOutputBytes,
		config.MaxCostMicrousd, config.ExecutionTimeout.Milliseconds(), string(capabilities), string(permissions),
		config.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert app config revision: %w", err)
	}
	return nil
}

func scanAppConfig(scanner rowScanner) (appconfig.Config, error) {
	var config appconfig.Config
	var enabled bool
	var executorConfig []byte
	var executionTimeoutMS int64
	var capabilitiesJSON, permissionsJSON, createdAt, updatedAt string
	if err := scanner.Scan(
		&config.AppID, &config.Revision, &config.Generation, &enabled, &config.ExecutorID,
		&executorConfig, &config.MaxSteps, &config.MaxCapabilityCalls, &config.MaxExecutionUnits,
		&config.MaxOutputBytes, &config.MaxCostMicrousd, &executionTimeoutMS,
		&capabilitiesJSON, &permissionsJSON, &createdAt, &updatedAt,
	); err != nil {
		return appconfig.Config{}, err
	}
	config.Enabled = enabled
	config.ExecutorConfig = append(json.RawMessage(nil), executorConfig...)
	config.ExecutionTimeout = time.Duration(executionTimeoutMS) * time.Millisecond
	if len(config.ExecutorConfig) > 64<<10 || len(capabilitiesJSON) > 65536 || len(permissionsJSON) > 65536 {
		return appconfig.Config{}, appconfig.ErrInvalid
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &config.EnabledCapabilities); err != nil {
		return appconfig.Config{}, errors.Join(appconfig.ErrInvalid, err)
	}
	if err := json.Unmarshal([]byte(permissionsJSON), &config.PermissionScope); err != nil {
		return appconfig.Config{}, errors.Join(appconfig.ErrInvalid, err)
	}
	var err error
	config.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return appconfig.Config{}, errors.Join(appconfig.ErrInvalid, err)
	}
	config.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return appconfig.Config{}, errors.Join(appconfig.ErrInvalid, err)
	}
	expectedRevision := config.Revision
	normalized, err := appconfig.Normalize(config)
	if err != nil || normalized.Revision != expectedRevision {
		return appconfig.Config{}, appconfig.ErrInvalid
	}
	normalized.Generation = config.Generation
	normalized.CreatedAt = config.CreatedAt
	normalized.UpdatedAt = config.UpdatedAt
	return normalized, nil
}
