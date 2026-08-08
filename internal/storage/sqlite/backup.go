package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const currentSchemaVersion = 14
const minimumRestorableSchemaVersion = 9

var (
	ErrBackupDestinationExists  = errors.New("backup destination already exists")
	ErrRestoreDestinationExists = errors.New("restore destination already exists")
	ErrInvalidBackup            = errors.New("invalid sqlite backup")
)

// Backup 创建由 SQLite 保证一致性的完整快照，并以不覆盖既有目标的方式原子发布。
func (s *Store) Backup(ctx context.Context, destination string) (resultErr error) {
	started := time.Now()
	destination, parent, err := validateNewDatabasePath(destination, ErrBackupDestinationExists)
	if err != nil {
		return err
	}
	temporary, err := reserveTemporaryPath(parent, ".ailuo-backup-")
	if err != nil {
		return fmt.Errorf("reserve backup path: %w", err)
	}
	published := false
	defer func() {
		if !published {
			if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove backup temporary file: %w", err))
			}
		}
	}()
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, temporary); err != nil {
		return fmt.Errorf("create sqlite backup: %w", err)
	}
	if err := hardenAndSyncFile(temporary); err != nil {
		return fmt.Errorf("sync sqlite backup: %w", err)
	}
	if err := ValidateBackup(ctx, temporary); err != nil {
		return err
	}
	if err := os.Link(temporary, destination); errors.Is(err, os.ErrExist) {
		return ErrBackupDestinationExists
	} else if err != nil {
		return fmt.Errorf("publish sqlite backup: %w", err)
	}
	published = true
	if err := os.Remove(temporary); err != nil {
		return fmt.Errorf("remove published backup temporary link: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync backup directory: %w", err)
	}
	observeStorageOperation(ctx, "backup", started, nil)
	return nil
}

// BackupDatabase 不执行迁移，适用于停机维护和升级前备份。
func BackupDatabase(ctx context.Context, source, destination string) error {
	source, err := validateExistingRegularFile(source)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return fmt.Errorf("open sqlite for backup: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	backupErr := store.Backup(ctx, destination)
	closeErr := db.Close()
	return errors.Join(backupErr, closeErr)
}

// ValidateBackup 以只读模式执行完整性、外键和迁移版本检查。
func ValidateBackup(ctx context.Context, path string) (resultErr error) {
	started := time.Now()
	absolute, err := validateExistingRegularFile(path)
	if err != nil {
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, err)
	}
	uri := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, fmt.Errorf("open backup read-only: %w", err))
	}
	defer func() {
		resultErr = errors.Join(resultErr, db.Close())
	}()
	db.SetMaxOpenConns(1)
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		if err == nil {
			err = fmt.Errorf("integrity check result is %q", integrity)
		}
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, fmt.Errorf("check backup integrity: %w", err))
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, fmt.Errorf("check backup foreign keys: %w", err))
	}
	hasViolation := rows.Next()
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if hasViolation || rowsErr != nil || closeErr != nil {
		err = errors.Join(rowsErr, closeErr)
		if err == nil {
			err = errors.New("foreign key violation")
		}
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, fmt.Errorf("backup foreign keys are invalid: %w", err))
	}
	var version, migrationCount int
	if err := db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0), count(*) FROM schema_migrations`).Scan(&version, &migrationCount); err != nil {
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, fmt.Errorf("read backup schema version: %w", err))
	}
	if version < minimumRestorableSchemaVersion || version > currentSchemaVersion || migrationCount != version {
		err := fmt.Errorf("schema migration history is outside the supported restore range")
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, err)
	}
	if err := validateRequiredSchema(ctx, db, version); err != nil {
		observeStorageOperation(ctx, "backup_validate", started, err)
		return errors.Join(ErrInvalidBackup, err)
	}
	observeStorageOperation(ctx, "backup_validate", started, nil)
	return nil
}

func validateRequiredSchema(ctx context.Context, db *sql.DB, version int) error {
	for _, table := range []string{
		"schema_migrations",
		"bus_source_revisions",
		"bus_stops",
		"bus_routes",
		"bus_journeys",
		"bus_current_snapshots",
		"echoes",
		"echo_events",
		"runs",
		"capability_audit",
		"idempotency_records",
		"echo_create_requests",
	} {
		query := "SELECT 1 FROM " + table + " LIMIT 0"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("required database table is unavailable")
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close required schema probe: %w", err)
		}
	}
	if version >= 13 {
		for _, table := range []string{"app_config_revisions", "app_config_heads"} {
			rows, err := db.QueryContext(ctx, "SELECT 1 FROM "+table+" LIMIT 0")
			if err != nil {
				return fmt.Errorf("required App configuration table is unavailable")
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close App configuration schema probe: %w", err)
			}
		}
	}
	runColumns := `
SELECT app_id,run_id,echo_id,attempt,status,model,model_config_version,protocol_version,
       max_steps,max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,
       max_cost_microusd,provider_timeout_ms,used_input_tokens,used_output_tokens,used_total_tokens,
       used_cost_microusd,last_agent_sequence
FROM runs LIMIT 0`
	if version >= 10 {
		runColumns = `
SELECT app_id,run_id,echo_id,attempt,status,model,model_config_version,protocol_version,
       max_steps,max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,
       max_cost_microusd,provider_timeout_ms,used_input_tokens,used_output_tokens,used_total_tokens,
       used_cost_microusd,used_provider_retries,last_agent_sequence
FROM runs LIMIT 0`
	}
	if version >= 11 {
		runColumns = `
SELECT app_id,run_id,echo_id,attempt,status,model,model_config_version,protocol_version,
       max_steps,max_tool_calls,max_input_tokens,max_output_tokens,max_total_tokens,max_output_bytes,
       max_cost_microusd,provider_timeout_ms,used_input_tokens,used_output_tokens,used_total_tokens,
       used_cost_microusd,used_provider_retries,available_at,last_agent_sequence
FROM runs LIMIT 0`
	}
	if version >= 14 {
		runColumns = `
SELECT app_id,run_id,run_group_id,echo_id,parent_run_id,origin_call_id,attempt,status,
       model,model_config_version,protocol_version,max_steps,max_tool_calls,max_input_tokens,
       max_output_tokens,max_total_tokens,max_output_bytes,max_cost_microusd,provider_timeout_ms,
       used_input_tokens,used_output_tokens,used_total_tokens,used_cost_microusd,
       used_provider_retries,available_at,capability_scope,permission_scope,result_message,
       last_agent_sequence
FROM runs LIMIT 0`
		rows, err := db.QueryContext(ctx, `SELECT app_id,run_id,call_id,echo_id FROM capability_audit LIMIT 0`)
		if err != nil {
			return fmt.Errorf("required Capability audit schema is unavailable")
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close Capability audit schema probe: %w", err)
		}
	}
	rows, err := db.QueryContext(ctx, runColumns)
	if err != nil {
		return fmt.Errorf("required Run schema is unavailable")
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close Run schema probe: %w", err)
	}
	return nil
}

// RestoreBackup 在目标不存在时恢复数据库；调用方必须确保目标数据库未被任何进程打开。
func RestoreBackup(ctx context.Context, backupPath, destination string) (resultErr error) {
	started := time.Now()
	backupPath, err := validateExistingRegularFile(backupPath)
	if err != nil {
		return errors.Join(ErrInvalidBackup, err)
	}
	if err := ValidateBackup(ctx, backupPath); err != nil {
		return err
	}
	destination, parent, err := validateNewDatabasePath(destination, ErrRestoreDestinationExists)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".ailuo-restore-")
	if err != nil {
		return fmt.Errorf("create restore temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		if err := temporary.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close restore temporary file: %w", err))
		}
		if !published {
			if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove restore temporary file: %w", err))
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("harden restore temporary file: %w", err)
	}
	source, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("open backup source: %w", err)
	}
	copyErr := copyWithContext(ctx, temporary, source)
	closeSourceErr := source.Close()
	if err := errors.Join(copyErr, closeSourceErr); err != nil {
		return fmt.Errorf("copy backup for restore: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync restored database: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close restored database: %w", err)
	}
	if err := ValidateBackup(ctx, temporaryPath); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, destination); errors.Is(err, os.ErrExist) {
		return ErrRestoreDestinationExists
	} else if err != nil {
		return fmt.Errorf("publish restored database: %w", err)
	}
	published = true
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove restored database temporary link: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync restore directory: %w", err)
	}
	observeStorageOperation(ctx, "restore", started, nil)
	return nil
}

func validateNewDatabasePath(path string, existsError error) (string, string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", "", errors.New("database destination must be an absolute path")
	}
	absolute := filepath.Clean(path)
	parent := filepath.Dir(absolute)
	info, err := os.Stat(parent)
	if err != nil {
		return "", "", fmt.Errorf("inspect database destination directory: %w", err)
	}
	if !info.IsDir() {
		return "", "", errors.New("database destination parent is not a directory")
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", "", existsError
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("inspect database destination: %w", err)
	}
	return absolute, parent, nil
}

func validateExistingRegularFile(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("database path must be absolute")
	}
	absolute := filepath.Clean(path)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect database file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", errors.New("database path is not a non-empty regular file")
	}
	return absolute, nil
}

func reserveTemporaryPath(parent, pattern string) (string, error) {
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func hardenAndSyncFile(path string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return file.Sync()
}

func syncDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close())
	}()
	return directory.Sync()
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if _, err := destination.Write(buffer[:read]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
