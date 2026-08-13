package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

// inputSchemaJSON 是 agent.run 的严格输入 Schema：任务正文与可选的 Capability
// 白名单（child Run 的接受期能力上界，只允许收窄）。
const inputSchemaJSON = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "required":["task"],
  "properties":{
    "task":{"type":"string","minLength":1,"maxLength":4000},
    "capability_ids":{
      "type":"array",
      "maxItems":16,
      "uniqueItems":true,
      "items":{"type":"string","pattern":"^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$","maxLength":128}
    }
  }
}`

// Runner 是 Agent Service 的 child Run 执行方（由内核 Orchestrator 实现）。
type Runner interface {
	RunChild(context.Context, echo.ChildRunRequest) (echo.ChildRunResult, error)
}

type input struct {
	Task          string   `json:"task"`
	CapabilityIDs []string `json:"capability_ids"`
}

// Register 把 Agent Service 注册进 Registry：ServiceID=agent、对外核心能力
// agent.run。该能力是外部副作用：必须携带幂等键并经 Dispatcher 治理后才执行；
// 运行方（Runner）由内核在 Orchestrator 构建后注入。
func Register(reg *registry.Registry, runner Runner) error {
	if reg == nil || runner == nil {
		return registry.ErrInvalidSpec
	}
	handler := func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		var value input
		if err := jsonutil.DecodeStrict(payload, &value); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		result, err := runner.RunChild(ctx, echo.ChildRunRequest{
			ParentRunID:     request.RunID,
			OriginCallID:    request.CallID,
			Task:            value.Task,
			CapabilityScope: value.CapabilityIDs,
		})
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("encode child Run result: %w", err)
		}
		return encoded, nil
	}
	return reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:          ServiceID,
			Version:     "1.0.0",
			Description: "Governed one-level child Agent Runs.",
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			CapabilityID: {
				Spec: registry.CapabilitySpec{
					ID:              CapabilityID,
					Version:         "1.0.0",
					Name:            "委派受治理的子任务",
					Description:     "Create one durable child Run with narrower Capabilities and an independent budget, then return its explicit result to the parent Run.",
					ServiceID:       ServiceID,
					InputSchemaJSON: inputSchemaJSON,
					SideEffect:      registry.SideEffectExternal,
				},
				Handler: handler,
			},
		},
	})
}
