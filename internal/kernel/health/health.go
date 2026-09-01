package health

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
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

// ExecutorChecker 检查执行者：协议协商与执行者自身的就绪状态。
// 由内核装配、Loader 的执行者运行时与集成测试共用。
type ExecutorChecker struct {
	Client executor.Client
}

func (h ExecutorChecker) Ping(ctx context.Context) error {
	started := time.Now()
	response, err := h.Client.Health(ctx, &executor.HealthRequest{
		AcceptedProtocolVersions: []string{executor.Version},
	})
	if err != nil {
		observe.Warn(ctx, "执行者健康检查失败",
			observe.Component("executor_health"),
			observe.Duration(started),
		)
		return fmt.Errorf("AI 执行者健康检查：%w", err)
	}
	if err := executor.ValidateHealthResponse(response); err != nil {
		return fmt.Errorf("执行者健康响应无效：%w", err)
	}
	if !executor.Supports(response.SupportedProtocolVersions) {
		return executor.ErrVersionMismatch
	}
	if !response.Ready {
		observe.Warn(ctx, "执行者尚未就绪",
			observe.Component("executor_health"),
			observe.Duration(started),
		)
		return fmt.Errorf("执行者尚未就绪")
	}
	observe.Debug(ctx, "执行者健康检查通过",
		observe.Component("executor_health"),
		observe.Duration(started),
	)
	return nil
}

// ExecutorAppChecker 按当前 App 启停策略检查执行者就绪。
type ExecutorAppChecker struct {
	Client executor.Client
	Source appconfig.Source
	AppID  string
}

func (h ExecutorAppChecker) Ping(ctx context.Context) error {
	config, err := h.Source.Current(ctx, h.AppID)
	if err != nil {
		return fmt.Errorf("读取 App 配置：%w", err)
	}
	if err := appconfig.VerifyCurrent(config, h.AppID); err != nil {
		return fmt.Errorf("当前 App 配置无效：%w", err)
	}
	if !config.Enabled {
		return fmt.Errorf("当前 App 已停用")
	}
	return ExecutorChecker{Client: h.Client}.Ping(ctx)
}
