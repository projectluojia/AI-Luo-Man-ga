package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const migrationTimeout = 60 * time.Second

type Store struct {
	db *sql.DB
	// txMu 按事务生命周期串行化全部写事务：modernc/sqlite 单连接并发事务会破坏
	// 事务边界（嵌套 BEGIN / 提交时语句进行中 / 并发语句报锁），事务必须显式互斥。
	txMu sync.Mutex
}

type rowScanner interface {
	Scan(...any) error
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type queryer interface {
	rowQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// beginTx 领取事务并持有串行化互斥，直到 finishTx 提交/回滚后释放。
func (s *Store) beginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	s.txMu.Lock()
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		s.txMu.Unlock()
		return nil, err
	}
	return tx, nil
}

// finishTx 结束事务并释放串行化互斥（回滚逻辑复用 rollbackTx）。
func (s *Store) finishTx(tx *sql.Tx, resultErr *error, operation string) {
	*resultErr = s.rollbackTx(tx, *resultErr, operation)
}

// rollbackTx 回滚事务并释放串行化互斥（beginTx 后的内联错误路径专用）。
func (s *Store) rollbackTx(tx *sql.Tx, primary error, operation string) error {
	err := rollbackTransaction(tx, primary, operation)
	s.txMu.Unlock()
	return err
}

func Open(path string) (*Store, error) {
	started := time.Now()
	// 单连接 + busy_timeout：连接上限与事务互斥（txMu）共同保证串行化；
	// busy_timeout 兜底跨进程写竞争。foreign_keys 经 DSN 对每个新连接生效。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), migrationTimeout)
	defer cancel()
	if err := store.migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	observe.Info(context.Background(), "SQLite 统一数据库已经打开",
		observe.Component("storage"),
		observe.Duration(started),
	)
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "ping", started, resultErr) }()
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	return s.migrateThrough(ctx, 0)
}

// registeredMigrations 是统一的前向迁移注册表，key 为唯一迁移版本号。
// 各存储实现通过 registerMigration 在包初始化阶段注册自身迁移，
// 使多个模块可以独立并行地扩展数据库 Schema，而不需要改动既有迁移。
var registeredMigrations = make(map[int]string)

// registerMigration 注册一个前向迁移。版本号必须唯一且大于 0；
// 重复或非法注册属于启动期编程错误，直接以显式错误终止启动。
func registerMigration(version int, statements string) {
	if version <= 0 {
		panic(fmt.Sprintf("非法迁移版本号 %d", version))
	}
	if _, exists := registeredMigrations[version]; exists {
		panic(fmt.Sprintf("迁移版本 %d 重复注册", version))
	}
	registeredMigrations[version] = statements
}

// currentSchemaVersion 返回当前注册的最大迁移版本，即当前数据库 Schema 版本。
func currentSchemaVersion() int {
	maximum := 0
	for version := range registeredMigrations {
		if version > maximum {
			maximum = version
		}
	}
	return maximum
}

func (s *Store) migrateThrough(ctx context.Context, maximumVersion int) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
		return fmt.Errorf("initialize sqlite migrations: %w", err)
	}
	// WAL 是持久化文件数据库的既定日志模式：只读/不支持 WAL 的文件系统会让
	// PRAGMA 静默退回回滚日志模式，必须校验实际生效模式并 fail-closed，不得
	// 在错误的持久性档案下继续运行。内存数据库只能为 memory 模式，无持久性
	// 要求，允许通过。
	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read sqlite journal mode: %w", err)
	}
	switch strings.ToLower(journalMode) {
	case "wal", "memory":
	default:
		return fmt.Errorf("sqlite journal mode is %q, expected wal (WAL 不可用时拒绝启动，避免静默降级)", journalMode)
	}
	versions := make([]int, 0, len(registeredMigrations))
	for version := range registeredMigrations {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	if len(versions) == 0 {
		return fmt.Errorf("no database migrations are registered")
	}
	if maximumVersion < 0 || (maximumVersion > 0 && versions[len(versions)-1] < maximumVersion) {
		return fmt.Errorf("invalid maximum migration version")
	}
	if maximumVersion == 0 {
		var applied, total int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, schemaBaselineVersion).Scan(&applied); err != nil {
			return fmt.Errorf("check schema baseline: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&total); err != nil {
			return fmt.Errorf("check schema history: %w", err)
		}
		if total > 0 && applied != 1 {
			return fmt.Errorf("SQLite 数据库仍使用旧开发版 Schema，请删除数据库并重新部署")
		}
		if total == 0 {
			if err := s.applyMigration(ctx, schemaBaselineVersion, registeredMigrations[schemaBaselineVersion]); err != nil {
				return err
			}
		}
	}
	for _, version := range versions {
		if maximumVersion == 0 && version < schemaBaselineVersion {
			continue
		}
		if maximumVersion > 0 && version > maximumVersion {
			continue
		}
		migration := registeredMigrations[version]
		var applied int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if applied > 0 {
			continue
		}
		if err := s.applyMigration(ctx, version, migration); err != nil {
			return err
		}
		observe.Info(ctx, "数据库迁移已经应用",
			observe.Component("storage"),
			observe.IntAttr("migration_version", version),
		)
	}
	return nil
}

// applyMigration 在独立事务中应用单个迁移；事务互斥由 finishTx 在返回时释放。
func (s *Store) applyMigration(ctx context.Context, version int, migration string) error {
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", version, err)
	}
	var resultErr error
	defer s.finishTx(tx, &resultErr, "apply migration")
	if _, err := tx.ExecContext(ctx, migration); err != nil {
		return errors.Join(fmt.Errorf("apply migration %d: %w", version, err), tx.Rollback())
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return errors.Join(fmt.Errorf("record migration %d: %w", version, err), tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", version, err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
