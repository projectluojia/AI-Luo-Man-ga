package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	ServiceID    = "agent"
	CapabilityID = echo.SubagentCapabilityID
)

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

type Runner interface {
	RunChild(context.Context, echo.ChildRunRequest) (echo.ChildRunResult, error)
}

type input struct {
	Task          string   `json:"task"`
	CapabilityIDs []string `json:"capability_ids"`
}

func Register(reg *registry.Registry, runner Runner) error {
	if reg == nil || runner == nil {
		return registry.ErrInvalidSpec
	}
	handler := func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		var value input
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		if err := ensureEOF(decoder); err != nil {
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

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return registry.ErrSchemaValidation
}
