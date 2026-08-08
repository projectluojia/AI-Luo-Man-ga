package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

func finishTransaction(tx *sql.Tx, resultErr *error, operation string) {
	*resultErr = rollbackTransaction(tx, *resultErr, operation)
}

func rollbackTransaction(tx *sql.Tx, primary error, operation string) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return errors.Join(primary, fmt.Errorf("%s rollback: %w", operation, err))
	}
	return primary
}

func observeStorageOperation(ctx context.Context, operation string, started time.Time, err error) {
	observe.DefaultMetrics().ObserveStorage(err == nil, time.Since(started))
	if err != nil {
		observe.Error(ctx, "SQLite 存储操作失败", err,
			observe.StringAttr("storage_operation", operation),
			observe.Duration(started),
		)
		return
	}
	observe.Debug(ctx, "SQLite 存储操作完成",
		observe.StringAttr("storage_operation", operation),
		observe.Duration(started),
	)
}
