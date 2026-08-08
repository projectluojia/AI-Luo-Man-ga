package agenthost

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type HealthChecker struct {
	client agentv1.AgentRuntimeClient
	model  string
	source appconfig.Source
	appID  string
}

func NewHealthChecker(client agentv1.AgentRuntimeClient, model string) *HealthChecker {
	return &HealthChecker{client: client, model: model}
}

func NewAppHealthChecker(client agentv1.AgentRuntimeClient, source appconfig.Source, appID string) *HealthChecker {
	return &HealthChecker{client: client, source: source, appID: appID}
}

func (h *HealthChecker) Ping(ctx context.Context) error {
	started := time.Now()
	model := h.model
	if h.source != nil {
		config, err := h.source.Current(ctx, h.appID)
		if err != nil {
			return fmt.Errorf("读取 App 模型配置：%w", err)
		}
		if err := appconfig.VerifyCurrent(config, h.appID); err != nil {
			return fmt.Errorf("App 模型配置无效：%w", err)
		}
		if !config.Enabled {
			return fmt.Errorf("当前 App 已停用")
		}
		model = config.Model
	}
	response, err := h.client.Health(ctx, &agentv1.HealthRequest{
		AcceptedProtocolVersions: []string{agentprotocol.Version},
		Model:                    model,
	})
	if err != nil {
		observe.Warn(ctx, "Python AI Agent 健康检查失败",
			observe.Component("agent_host"),
			observe.Duration(started),
		)
		return fmt.Errorf("Python AI Agent 健康检查：%w", err)
	}
	if !agentprotocol.Supports(response.SupportedProtocolVersions) {
		return agentprotocol.ErrVersionMismatch
	}
	if !response.Ready {
		observe.Warn(ctx, "Python AI Agent 尚未就绪",
			observe.Component("agent_host"),
			observe.Duration(started),
		)
		return fmt.Errorf("Python AI Agent 尚未就绪")
	}
	observe.Debug(ctx, "Python AI Agent 健康检查通过",
		observe.Component("agent_host"),
		observe.StringAttr("provider", response.Provider),
		observe.Duration(started),
	)
	return nil
}
