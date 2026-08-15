package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	ServiceID         = "prompt"
	PreferenceGetID   = "prompt.preference.get"
	PreferenceSetID   = "prompt.preference.set"
	PreferenceResetID = "prompt.preference.reset"
)

const emptyInputSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false
}`

const setInputSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "basic_style":{
      "type":"string",
      "enum":[
        "default","默认","professional","专业可靠","friendly","亲和友善",
        "direct","直言不讳","imaginative","天马行空","pragmatic","高效务实",
        "roast","吐槽达人"
      ]
    },
    "extra_trait_levels":{
      "type":"object",
      "additionalProperties":false,
      "properties":{
        "considerate":{"type":"string","enum":["enhanced","default","reduced","strong","increase","normal","增强","默认","减弱"]},
        "enthusiastic":{"type":"string","enum":["enhanced","default","reduced","strong","increase","normal","增强","默认","减弱"]},
        "emoji":{"type":"string","enum":["enhanced","default","reduced","strong","increase","normal","增强","默认","减弱"]},
        "headings_lists":{"type":"string","enum":["enhanced","default","reduced","strong","increase","normal","增强","默认","减弱"]}
      }
    }
  }
}`

type setInput struct {
	BasicStyle       string            `json:"basic_style"`
	ExtraTraitLevels map[string]string `json:"extra_trait_levels"`
}

// Register 把 prompt Service 注册进 Registry。偏好读取是 read 副作用，设置与
// 重置是 write 副作用（走 Dispatcher 的幂等与审计治理）。渲染入口 RenderSystemPrompt
// 是内核系统端口，不注册为模型可见 Capability。
func Register(reg *registry.Registry, service *Service) error {
	if reg == nil || service == nil {
		return registry.ErrInvalidSpec
	}
	return reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:          ServiceID,
			Version:     "1.0.0",
			Description: "User prompt style preferences migrated from LuoYingRebuild V2.",
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			PreferenceGetID: {
				Spec: registry.CapabilitySpec{
					ID:              PreferenceGetID,
					Version:         "1.0.0",
					Name:            "查看我的提示词偏好",
					Description:     "Get the current user's prompt style preference, including basic style and extra trait levels.",
					ServiceID:       ServiceID,
					InputSchemaJSON: emptyInputSchema,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: service.getPreference,
			},
			PreferenceSetID: {
				Spec: registry.CapabilitySpec{
					ID:              PreferenceSetID,
					Version:         "1.0.0",
					Name:            "设置我的提示词偏好",
					Description:     "Set the current user's prompt style preference. basic_style accepts the style key or Chinese name; extra_trait_levels accepts trait key/Chinese name to enhanced/default/reduced level.",
					ServiceID:       ServiceID,
					InputSchemaJSON: setInputSchema,
					SideEffect:      registry.SideEffectWrite,
				},
				Handler: service.setPreference,
			},
			PreferenceResetID: {
				Spec: registry.CapabilitySpec{
					ID:              PreferenceResetID,
					Version:         "1.0.0",
					Name:            "重置我的提示词偏好",
					Description:     "Reset the current user's prompt style preference back to V2 defaults.",
					ServiceID:       ServiceID,
					InputSchemaJSON: emptyInputSchema,
					SideEffect:      registry.SideEffectWrite,
				},
				Handler: service.resetPreference,
			},
		},
	})
}

func (s *Service) getPreference(ctx context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
	settings, err := s.Settings(ctx, request.AppID, request.UserID)
	if err != nil {
		return nil, err
	}
	levels := settings.ExtraTraitLevels
	if levels == nil {
		levels = map[string]string{}
	}
	encoded, err := json.Marshal(map[string]any{
		"basic_style":        settings.BasicStyle,
		"extra_trait_levels": levels,
		"message":            "这是你当前的提示词偏好。",
	})
	if err != nil {
		return nil, fmt.Errorf("encode prompt preference: %w", err)
	}
	return encoded, nil
}

func (s *Service) setPreference(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	var input setInput
	if err := jsonutil.DecodeStrict(payload, &input); err != nil {
		return nil, errors.Join(registry.ErrSchemaValidation, err)
	}
	if input.BasicStyle == "" && len(input.ExtraTraitLevels) == 0 {
		return nil, fmt.Errorf("%w: basic_style or extra_trait_levels is required", ErrInvalid)
	}
	settings, err := s.SetSettings(ctx, request.AppID, request.UserID, input.BasicStyle, input.ExtraTraitLevels)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(map[string]any{
		"basic_style":        settings.BasicStyle,
		"extra_trait_levels": settings.ExtraTraitLevels,
		"message":            "已更新你的提示词偏好。",
	})
	if err != nil {
		return nil, fmt.Errorf("encode prompt preference: %w", err)
	}
	return encoded, nil
}

func (s *Service) resetPreference(ctx context.Context, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
	if err := s.ResetSettings(ctx, request.AppID, request.UserID); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"message":"已重置你的提示词偏好。"}`), nil
}
