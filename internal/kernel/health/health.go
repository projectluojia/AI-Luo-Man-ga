package health

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type Checker interface {
	Ping(context.Context) error
}

type Combined []Checker

func (c Combined) Ping(ctx context.Context) error {
	started := time.Now()
	for index, checker := range c {
		if err := checker.Ping(ctx); err != nil {
			observe.Warn(ctx, "依赖项健康检查未通过",
				observe.IntAttr("checker_index", index),
				observe.StringAttr("result_status", "unavailable"),
				observe.Duration(started),
			)
			return fmt.Errorf("dependency %d: %w", index, err)
		}
	}
	observe.Debug(ctx, "全部依赖项健康检查通过",
		observe.IntAttr("checker_count", len(c)),
		observe.Duration(started),
	)
	return nil
}
