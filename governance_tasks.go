package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// 治理类后台任务的清扫周期。确认默认有效期 30 分钟，5 分钟一轮足以兜底；
// 清扫幂等（重复执行只影响已到期记录），失败不自动重试，由下一轮周期补上。
const confirmationSweepInterval = 5 * time.Minute

// confirmationSweepType 是"确认过期清扫"封闭任务类型：周期性把超期的
// waiting/approved 确认标记为 expired，防止未决确认无限堆积。
const confirmationSweepType = "governance.confirmation.expiry"

// sweepParams 与 sweepParamsSchema 是清扫任务的固定参数：严格 Schema 要求空对象。
var (
	sweepParams       = json.RawMessage(`{}`)
	sweepParamsSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
)

// confirmationSweepRequest 构造一轮确认过期清扫任务：周期对齐的幂等键保证
// 同一轮次不会重复排队，可用时间与期限固定。
func confirmationSweepRequest(appID string, nextAvailable time.Time) task.CreateRequest {
	return task.CreateRequest{
		AppID:          appID,
		Type:           confirmationSweepType,
		Params:         sweepParams,
		IdempotencyKey: "confirmation.expiry." + nextAvailable.Truncate(confirmationSweepInterval).UTC().Format(time.RFC3339),
		Deadline:       nextAvailable.Add(2 * time.Hour),
		AvailableAt:    nextAvailable,
	}
}

// registerGovernanceTaskTypes 注册治理类后台任务类型。清扫处理器在每次执行后
// 无条件安排下一轮（固定周期），保证清扫链不因单次失败中断；本次清扫失败会
// 反映到任务失败状态与日志中，便于观测。
func registerGovernanceTaskTypes(types *task.TypeRegistry, confirmations *confirmation.Service, scheduler *task.Scheduler, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("invalid confirmation sweep interval %s", interval)
	}
	return types.Register(task.TypeSpec{
		TypeID:       confirmationSweepType,
		ParamsSchema: sweepParamsSchema,
		AllowRetry:   false,
		Handler: func(ctx context.Context, value task.Task) error {
			affected, sweepErr := confirmations.ExpireDue(ctx, value.AppID, time.Now().UTC())
			if sweepErr != nil {
				observe.Error(ctx, "确认过期清扫失败", sweepErr,
					observe.StringAttr("app_id", value.AppID),
				)
			} else if affected > 0 {
				observe.Info(ctx, "确认过期清扫完成",
					observe.StringAttr("app_id", value.AppID),
					observe.Int64Attr("expired_count", affected),
				)
			}
			// 无条件安排下一轮：清扫链的生命周期独立于单次成败。
			_, scheduleErr := scheduler.Create(ctx, confirmationSweepRequest(value.AppID, time.Now().UTC().Add(interval)))
			if scheduleErr != nil {
				observe.Error(ctx, "安排下一轮确认过期清扫失败", scheduleErr,
					observe.StringAttr("app_id", value.AppID),
				)
			}
			return sweepErr
		},
	})
}

// seedConfirmationSweep 在启动时播种第一轮清扫任务。首个周期从 interval 后开始，
// 避免进程刚起来就立刻清扫一遍。
func seedConfirmationSweep(ctx context.Context, scheduler *task.Scheduler, appID string, interval time.Duration) error {
	_, err := scheduler.Create(ctx, confirmationSweepRequest(appID, time.Now().UTC().Add(interval)))
	return err
}
