package echo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	childServiceID = "run"

	childRunInputSchema = `{
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

	childStatusInputSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "required":["run_id"],
  "properties":{
    "run_id":{"type":"string","minLength":1,"maxLength":128}
  }
}`
)

// ChildRunner 是 Core 创建和读取受治理 child Run 的唯一端口。
type ChildRunner interface {
	RunChild(context.Context, ChildRunRequest) (ChildRunResult, error)
	GetChild(context.Context, ChildStatusRequest) (ChildStatusResult, error)
}

type childRunInput struct {
	Task          string   `json:"task"`
	CapabilityIDs []string `json:"capability_ids"`
}

type childStatusInput struct {
	RunID string `json:"run_id"`
}

// RegisterChildCapabilities 注册 Core 自己拥有的 child Run 控制能力。
// Executor 只能通过普通 CapabilityCall 使用它，不能在进程内私建子任务。
func RegisterChildCapabilities(reg *registry.Registry, runner ChildRunner) error {
	if reg == nil || runner == nil {
		return registry.ErrInvalidSpec
	}
	createChild := func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		var input childRunInput
		if err := jsonutil.DecodeStrict(payload, &input); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		result, err := runner.RunChild(ctx, ChildRunRequest{
			ParentRunID:     request.RunID,
			OriginCallID:    request.CallID,
			Task:            input.Task,
			CapabilityScope: input.CapabilityIDs,
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
	getChildStatus := func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		var input childStatusInput
		if err := jsonutil.DecodeStrict(payload, &input); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		result, err := runner.GetChild(ctx, ChildStatusRequest{
			ParentRunID: request.RunID,
			RunID:       input.RunID,
		})
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("encode child Run status: %w", err)
		}
		return encoded, nil
	}
	return reg.RegisterService(registry.ServiceRegistration{
		Spec: capability.ServiceSpec{
			ID: childServiceID, Version: "1.0.0", Description: "Governed child Run control.",
		},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			CreateChildRunCapabilityID: {
				Spec: capability.CapabilitySpec{
					ID:              CreateChildRunCapabilityID,
					Version:         "1.0.0",
					Name:            "创建受治理的子运行",
					Description:     "Create one durable queued child Run with narrower Capabilities and an independent budget.",
					ServiceID:       childServiceID,
					InputSchemaJSON: childRunInputSchema,
					SideEffect:      capability.SideEffectExternal,
				},
				Handler: createChild,
			},
			GetChildStatusCapabilityID: {
				Spec: capability.CapabilitySpec{
					ID:              GetChildStatusCapabilityID,
					Version:         "1.0.0",
					Name:            "查询子运行状态",
					Description:     "Read the durable status and completed result of a direct child Run.",
					ServiceID:       childServiceID,
					InputSchemaJSON: childStatusInputSchema,
					SideEffect:      capability.SideEffectNone,
				},
				Handler: getChildStatus,
			},
		},
	})
}
