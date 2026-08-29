package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const confirmationSweepType = "governance.confirmation.expiry"

var (
	sweepParams       = json.RawMessage(`{}`)
	sweepParamsSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
)

func confirmationSweepRequest(appID string, interval time.Duration, nextAvailable time.Time) task.CreateRequest {
	return task.CreateRequest{
		AppID: appID, Type: confirmationSweepType, Params: sweepParams,
		IdempotencyKey: "confirmation.expiry." + nextAvailable.Truncate(interval).UTC().Format(time.RFC3339),
		Deadline:       nextAvailable.Add(2 * time.Hour), AvailableAt: nextAvailable,
	}
}

func registerGovernanceTaskTypes(types *task.TypeRegistry, confirmations *confirmation.Service, scheduler *task.Scheduler, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("invalid confirmation sweep interval %s", interval)
	}
	return types.Register(task.TypeSpec{
		TypeID: confirmationSweepType, ParamsSchema: sweepParamsSchema, AllowRetry: false,
		Handler: func(ctx context.Context, value task.Task) error {
			affected, sweepErr := confirmations.ExpireDue(ctx, value.AppID, time.Now().UTC())
			if sweepErr != nil {
				observe.Error(ctx, "确认过期清扫失败", sweepErr, observe.StringAttr("app_id", value.AppID))
			} else if affected > 0 {
				observe.Info(ctx, "确认过期清扫完成",
					observe.StringAttr("app_id", value.AppID), observe.Int64Attr("expired_count", affected))
			}
			_, scheduleErr := scheduler.Create(ctx, confirmationSweepRequest(value.AppID, interval, time.Now().UTC().Add(interval)))
			if scheduleErr != nil {
				observe.Error(ctx, "安排下一轮确认过期清扫失败", scheduleErr, observe.StringAttr("app_id", value.AppID))
			}
			return sweepErr
		},
	})
}

func seedConfirmationSweep(ctx context.Context, scheduler *task.Scheduler, appID string, interval time.Duration) error {
	_, err := scheduler.Create(ctx, confirmationSweepRequest(appID, interval, time.Now().UTC().Add(interval)))
	return err
}
