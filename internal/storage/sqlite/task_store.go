package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
)

// taskIDPattern 与 internal/kernel/task 中的稳定标识规则保持一致，
// 作为存储边界的防御性二次校验。
var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// 编译期保证：SQLite 适配器完整实现后台任务 Store 端口。
var _ task.Store = (*Store)(nil)

func init() {
	// 版本 15/16/17 占位迁移：身份（15）、会话（16）、确认（17）三个模块
	// 处于并行开发分支，尚未并入本分支。为保证 schema_migrations 版本连续
	// （ValidateBackup 要求 migrationCount==version），按会话模块已建立的占位
	// 约定临时占用这三个版本号；对应模块的正式迁移并入后必须删除占位注册，
	// 否则会重复注册并以显式错误终止启动。
	registerMigration(15, `CREATE TEMP TABLE IF NOT EXISTS task_module_gap_placeholder_15 (id INTEGER PRIMARY KEY);`)
	registerMigration(16, `CREATE TEMP TABLE IF NOT EXISTS task_module_gap_placeholder_16 (id INTEGER PRIMARY KEY);`)
	registerMigration(17, `CREATE TEMP TABLE IF NOT EXISTS task_module_gap_placeholder_17 (id INTEGER PRIMARY KEY);`)
	// 迁移 18：后台任务持久状态机。状态迁移全部以租约令牌与租约到期条件
	// 做原子守卫；CHECK 约束在 SQL 边界强制状态机与参数的合法取值范围。
	registerMigration(18, `
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
CREATE INDEX tasks_queue_idx ON tasks(app_id, status, available_at);
CREATE INDEX tasks_lease_idx ON tasks(status, lease_expires_at);
CREATE INDEX tasks_app_lease_idx ON tasks(app_id, status, lease_expires_at);
`)
}

// taskSelectColumns 是 tasks 表投影列，同时用于 SELECT 与 UPDATE ... RETURNING。
const taskSelectColumns = `
  app_id, task_id, task_type, status, attempt, max_attempts,
  deadline_at, available_at, coalesce(lease_token, ''), lease_expires_at,
  idempotency_key, params, error_class, created_at, updated_at`

const taskSelect = "SELECT" + taskSelectColumns + " FROM tasks"

// scanTask 将一行 tasks 扫描为内核任务记录，并对持久化字段做边界校验。
func scanTask(scanner interface{ Scan(dest ...any) error }) (task.Task, error) {
	var value task.Task
	var attempt int64
	var maxAttempts int64
	var deadlineAt string
	var availableAt string
	var leaseExpiresAt sql.NullString
	var params []byte
	var errorClass string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&value.AppID, &value.TaskID, &value.Type, &value.Status, &attempt, &maxAttempts,
		&deadlineAt, &availableAt, &value.LeaseToken, &leaseExpiresAt,
		&value.IdempotencyKey, &params, &errorClass, &createdAt, &updatedAt,
	); err != nil {
		return task.Task{}, err
	}
	if attempt < 1 || maxAttempts < 1 || maxAttempts > 32 || attempt > maxAttempts {
		return task.Task{}, task.ErrInvalidTask
	}
	value.Attempt = uint32(attempt)
	value.MaxAttempts = uint32(maxAttempts)
	value.Params = params
	value.ErrorClass = task.ErrorClass(errorClass)
	var err error
	if value.Deadline, err = time.Parse(time.RFC3339Nano, deadlineAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task deadline: %w", err)
	}
	if value.AvailableAt, err = time.Parse(time.RFC3339Nano, availableAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task availability: %w", err)
	}
	if value.LeaseExpiresAt, err = parseOptionalTime(leaseExpiresAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task lease expiry: %w", err)
	}
	if value.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task creation time: %w", err)
	}
	if value.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return task.Task{}, fmt.Errorf("parse task update time: %w", err)
	}
	return value, nil
}

func scanTasks(rows *sql.Rows, kind string) ([]task.Task, error) {
	values := make([]task.Task, 0)
	for rows.Next() {
		value, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan %s task: %w", kind, err)
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close %s tasks: %w", kind, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s tasks: %w", kind, err)
	}
	return values, nil
}

// validateTaskIdentity 校验任务复合主键的合法形态。
func validateTaskIdentity(appID, taskID string) error {
	if len(appID) == 0 || len(appID) > 128 ||
		len(taskID) == 0 || len(taskID) > 128 || !taskIDPattern.MatchString(taskID) {
		return task.ErrInvalidTask
	}
	return nil
}

// validateFailureClass 校验可持久化的失败错误分类；None 不能作为失败终态分类。
func validateFailureClass(errorClass task.ErrorClass) error {
	switch errorClass {
	case task.ErrorClassRetryable, task.ErrorClassNonRetryable,
		task.ErrorClassDeadline, task.ErrorClassLeaseLost, task.ErrorClassCancelled:
		return nil
	default:
		return task.ErrInvalidTask
	}
}

// leaseGuardedWrite 执行一次以租约守卫的状态迁移写，返回受影响行数。
func (s *Store) leaseGuardedWrite(ctx context.Context, statement string, args ...any) (int64, error) {
	result, err := s.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// classifyLeaseFailure 在租约守卫写入未命中时读取当前状态，区分“租约丢失”
// （任务仍为 running，但租约已过期或令牌不符）与“状态非法”（已进入其他状态）。
func (s *Store) classifyLeaseFailure(ctx context.Context, appID, taskID string) error {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE app_id=? AND task_id=?`, appID, taskID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return task.ErrInvalidTransition
	}
	if err != nil {
		return fmt.Errorf("read task state after lease guard failure: %w", err)
	}
	if status == task.StatusRunning {
		return task.ErrLeaseLost
	}
	return task.ErrInvalidTransition
}

func (s *Store) CreateTask(ctx context.Context, value task.Task) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_task", started, resultErr) }()
	if err := task.ValidateNewTask(value); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tasks(
  app_id, task_id, task_type, status, attempt, max_attempts,
  deadline_at, available_at, idempotency_key, params, error_class, created_at, updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		value.AppID, value.TaskID, value.Type, value.Status, value.Attempt, value.MaxAttempts,
		value.Deadline.UTC().Format(time.RFC3339Nano), value.AvailableAt.UTC().Format(time.RFC3339Nano),
		value.IdempotencyKey, string(value.Params), string(value.ErrorClass),
		value.CreatedAt.UTC().Format(time.RFC3339Nano), value.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return task.ErrTaskExists
		}
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func (s *Store) ClaimTask(ctx context.Context, appID, taskID, leaseToken string, startedAt, leaseExpiresAt time.Time) (_ task.Task, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "claim_task", started, resultErr) }()
	if err := validateTaskIdentity(appID, taskID); err != nil {
		return task.Task{}, err
	}
	if leaseToken == "" || startedAt.IsZero() || !leaseExpiresAt.After(startedAt) {
		return task.Task{}, task.ErrInvalidTask
	}
	// 单条 UPDATE 原子领取：只有到期且未被领取的任务会被切换为 running。
	value, err := scanTask(s.db.QueryRowContext(ctx, `
UPDATE tasks
SET status=?, lease_token=?, lease_expires_at=?, updated_at=?
WHERE app_id=? AND task_id=? AND status IN (?,?) AND lease_token IS NULL
  AND julianday(available_at) <= julianday(?)
RETURNING`+taskSelectColumns,
		task.StatusRunning, leaseToken, leaseExpiresAt.UTC().Format(time.RFC3339Nano), startedAt.UTC().Format(time.RFC3339Nano),
		appID, taskID, task.StatusQueued, task.StatusRetryScheduled, startedAt.UTC().Format(time.RFC3339Nano),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.ErrInvalidTransition
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("claim task: %w", err)
	}
	return value, nil
}

func (s *Store) RenewTaskLease(ctx context.Context, value task.Task, renewedAt, leaseExpiresAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "renew_task_lease", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" ||
		renewedAt.IsZero() || !leaseExpiresAt.After(renewedAt) {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks SET lease_expires_at=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) >= julianday(?)`,
		leaseExpiresAt.UTC().Format(time.RFC3339Nano), renewedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		renewedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("renew task lease: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) CompleteTask(ctx context.Context, value task.Task, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "complete_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, lease_token=NULL, lease_expires_at=NULL, error_class=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) >= julianday(?)`,
		task.StatusSucceeded, "", completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("complete task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) FailTask(ctx context.Context, value task.Task, errorClass task.ErrorClass, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "fail_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if err := validateFailureClass(errorClass); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, lease_token=NULL, lease_expires_at=NULL, error_class=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) >= julianday(?)`,
		task.StatusFailed, string(errorClass), completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("fail task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) RetryTask(ctx context.Context, value task.Task, nextAvailableAt, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "retry_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() ||
		!nextAvailableAt.After(completedAt) || value.Attempt >= value.MaxAttempts {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, attempt=attempt+1, lease_token=NULL, lease_expires_at=NULL,
    available_at=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) >= julianday(?)`,
		task.StatusRetryScheduled, nextAvailableAt.UTC().Format(time.RFC3339Nano), completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("retry task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) CancelQueuedTask(ctx context.Context, appID, taskID string, completedAt time.Time) (_ task.Task, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_queued_task", started, resultErr) }()
	if err := validateTaskIdentity(appID, taskID); err != nil {
		return task.Task{}, false, err
	}
	if completedAt.IsZero() {
		return task.Task{}, false, task.ErrInvalidTask
	}
	value, err := scanTask(s.db.QueryRowContext(ctx, `
UPDATE tasks
SET status=?, lease_token=NULL, lease_expires_at=NULL, error_class=?, updated_at=?
WHERE app_id=? AND task_id=? AND status IN (?,?)
RETURNING`+taskSelectColumns,
		task.StatusCancelled, string(task.ErrorClassCancelled), completedAt.UTC().Format(time.RFC3339Nano),
		appID, taskID, task.StatusQueued, task.StatusRetryScheduled,
	))
	if errors.Is(err, sql.ErrNoRows) {
		// 未命中说明任务正在运行或已进入终态：这不是存储错误，返回取消结果 false。
		current, getErr := s.GetTask(ctx, appID, taskID)
		if getErr != nil {
			if errors.Is(getErr, task.ErrTaskNotFound) {
				return task.Task{}, false, task.ErrTaskNotFound
			}
			return task.Task{}, false, getErr
		}
		return current, false, nil
	}
	if err != nil {
		return task.Task{}, false, fmt.Errorf("cancel queued task: %w", err)
	}
	return value, true, nil
}

func (s *Store) CancelRunningTask(ctx context.Context, value task.Task, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_running_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, lease_token=NULL, lease_expires_at=NULL, error_class=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) >= julianday(?)`,
		task.StatusCancelled, string(task.ErrorClassCancelled), completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("cancel running task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) ListDueTasks(ctx context.Context, now time.Time, limit int) (_ []task.Task, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_due_tasks", started, resultErr) }()
	if now.IsZero() || limit < 1 || limit > 1000 {
		return nil, task.ErrInvalidTask
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+`
WHERE status IN (?,?) AND lease_token IS NULL AND julianday(available_at) <= julianday(?)
ORDER BY julianday(available_at), julianday(created_at), task_id LIMIT ?`,
		task.StatusQueued, task.StatusRetryScheduled, now.UTC().Format(time.RFC3339Nano), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query due tasks: %w", err)
	}
	return scanTasks(rows, "due")
}

func (s *Store) ListDeadTasks(ctx context.Context, now time.Time, limit int) (_ []task.Task, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_dead_tasks", started, resultErr) }()
	if now.IsZero() || limit < 1 || limit > 1000 {
		return nil, task.ErrInvalidTask
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+`
WHERE status=? AND lease_token IS NOT NULL AND julianday(lease_expires_at) < julianday(?)
ORDER BY julianday(lease_expires_at), julianday(created_at), task_id LIMIT ?`,
		task.StatusRunning, now.UTC().Format(time.RFC3339Nano), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query dead tasks: %w", err)
	}
	return scanTasks(rows, "dead")
}

// retryDeadTask 将租约已过期的运行中任务安排退避重试。
// 恢复路径的守卫是“租约已过期”，而不是“租约仍有效”。
func (s *Store) RetryDeadTask(ctx context.Context, value task.Task, nextAvailableAt, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "retry_dead_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() ||
		!nextAvailableAt.After(completedAt) || value.Attempt >= value.MaxAttempts {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, attempt=attempt+1, lease_token=NULL, lease_expires_at=NULL,
    available_at=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) < julianday(?)`,
		task.StatusRetryScheduled, nextAvailableAt.UTC().Format(time.RFC3339Nano), completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("retry dead task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) FailDeadTask(ctx context.Context, value task.Task, errorClass task.ErrorClass, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "fail_dead_task", started, resultErr) }()
	if err := validateTaskIdentity(value.AppID, value.TaskID); err != nil {
		return err
	}
	if err := validateFailureClass(errorClass); err != nil {
		return err
	}
	if value.Status != task.StatusRunning || value.LeaseToken == "" || completedAt.IsZero() {
		return task.ErrInvalidTask
	}
	affected, err := s.leaseGuardedWrite(ctx, `
UPDATE tasks
SET status=?, lease_token=NULL, lease_expires_at=NULL, error_class=?, updated_at=?
WHERE app_id=? AND task_id=? AND status=? AND lease_token=?
  AND julianday(lease_expires_at) < julianday(?)`,
		task.StatusFailed, string(errorClass), completedAt.UTC().Format(time.RFC3339Nano),
		value.AppID, value.TaskID, task.StatusRunning, value.LeaseToken,
		completedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("fail dead task: %w", err)
	}
	if affected != 1 {
		return s.classifyLeaseFailure(ctx, value.AppID, value.TaskID)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, appID, taskID string) (_ task.Task, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_task", started, resultErr) }()
	if err := validateTaskIdentity(appID, taskID); err != nil {
		return task.Task{}, err
	}
	value, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE app_id=? AND task_id=?`, appID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.ErrTaskNotFound
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("get task: %w", err)
	}
	return value, nil
}

func (s *Store) ListTasks(ctx context.Context, appID string, limit int) (_ []task.Task, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_tasks", started, resultErr) }()
	if len(appID) == 0 || len(appID) > 128 || limit < 1 || limit > 1000 {
		return nil, task.ErrInvalidTask
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE app_id=? ORDER BY julianday(created_at), task_id LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("query app tasks: %w", err)
	}
	return scanTasks(rows, "app")
}
