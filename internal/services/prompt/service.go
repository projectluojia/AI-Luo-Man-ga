// Package prompt 是 L3 提示词 Service。
//
// 它拥有 V2 迁移来的提示词个性化能力：基本风格、额外特征目录与用户偏好渲染。
// 基础人格、系统指令和渠道规则仍由 App 配置治理；本 Service 只负责把用户可选
// 的个性化片段按目录渲染出来，内核再把渲染结果交给 contextasm 做最终装配。
package prompt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
)

var ErrNotFound = errors.New("prompt settings not found")

// SettingsStore 是用户提示词偏好的持久化端口，由 Go 托管的存储层实现。
type SettingsStore interface {
	GetPromptSettings(context.Context, string, string) (Settings, error)
	SavePromptSettings(context.Context, string, Settings) error
	DeletePromptSettings(context.Context, string, string) error
}

// RenderRequest 是内核向提示词 Service 发出的受治理渲染请求。
// 所有输入已经由内核从持久配置或 Run 记录中投影，Service 不直接读库。
type RenderRequest struct {
	AppID            string
	UserID           string
	BaseSystemPrompt string
	Channel          string
	ChannelPrompts   map[string]string
}

type Service struct {
	catalog  promptcatalog.Catalog
	settings SettingsStore
}

func NewService(catalog promptcatalog.Catalog, settings SettingsStore) *Service {
	if catalog.IsZero() {
		catalog = promptcatalog.Default()
	}
	return &Service{catalog: catalog, settings: settings}
}

// RenderSystemPrompt 按用户偏好渲染基础提示 + 个性化片段 + 渠道提示。
// 用户未设置偏好或请求中没有用户时使用默认偏好；动态上下文（时间、历史、
// Capability 投影）不属于本方法，仍由内核 contextasm 继续装配。
func (s *Service) RenderSystemPrompt(ctx context.Context, request RenderRequest) (string, error) {
	base := strings.TrimSpace(request.BaseSystemPrompt)
	if base == "" {
		return "", ErrInvalid
	}
	settings := DefaultSettings(request.UserID)
	if request.UserID != "" && s.settings != nil {
		stored, err := s.settings.GetPromptSettings(ctx, request.AppID, request.UserID)
		switch {
		case err == nil:
			settings = NormalizeSettings(stored)
		case errors.Is(err, ErrNotFound):
		// 未设置过：继续使用默认偏好。
		default:
			return "", err
		}
	}

	parts := make([]string, 0, 4)
	parts = append(parts, base)
	if section := s.renderBasicStyle(settings); section != "" {
		parts = append(parts, section)
	}
	if section := s.renderExtraTraits(settings); section != "" {
		parts = append(parts, section)
	}
	if channel := strings.TrimSpace(request.ChannelPrompts[request.Channel]); channel != "" {
		parts = append(parts, channel)
	}
	return strings.Join(parts, "\n\n"), nil
}

func (s *Service) renderBasicStyle(settings Settings) string {
	key, err := NormalizeBasicStyle(settings.BasicStyle)
	if err != nil {
		key = "default"
	}
	style, ok := s.basicStyle(key)
	if !ok {
		return ""
	}
	return "【基本风格与语调】\n" + style.Name + "：" + style.Text
}

func (s *Service) renderExtraTraits(settings Settings) string {
	lines := make([]string, 0, len(s.catalog.ExtraTraits)+1)
	for _, trait := range s.catalog.ExtraTraits {
		level, ok := settings.ExtraTraitLevels[trait.Key]
		if !ok {
			level = "default"
		}
		text, ok := traitText(trait, level)
		if !ok {
			continue
		}
		lines = append(lines, "- "+text)
	}
	if len(lines) == 0 {
		return ""
	}
	return "【额外特征】\n" + strings.Join(lines, "\n")
}

func (s *Service) basicStyle(key string) (promptcatalog.BasicStyle, bool) {
	for _, style := range s.catalog.BasicStyles {
		if style.Key == key {
			return style, true
		}
	}
	return promptcatalog.BasicStyle{}, false
}

func traitText(trait promptcatalog.ExtraTrait, level string) (string, bool) {
	switch level {
	case "enhanced":
		return trait.Enhanced, trait.Enhanced != ""
	case "reduced":
		return trait.Reduced, trait.Reduced != ""
	default:
		return trait.Default, trait.Default != ""
	}
}

// Settings 返回用户当前偏好；未设置过返回默认偏好。
func (s *Service) Settings(ctx context.Context, appID, userID string) (Settings, error) {
	if userID == "" {
		return Settings{}, fmt.Errorf("%w: user is required", ErrInvalid)
	}
	if s.settings == nil {
		return DefaultSettings(userID), nil
	}
	stored, err := s.settings.GetPromptSettings(ctx, appID, userID)
	if errors.Is(err, ErrNotFound) {
		return DefaultSettings(userID), nil
	}
	if err != nil {
		return Settings{}, err
	}
	return NormalizeSettings(stored), nil
}

// SetSettings 保存用户偏好。输入先归一化，未知键回退默认。
func (s *Service) SetSettings(ctx context.Context, appID, userID string, basicStyle string, extraTraitLevels map[string]string) (Settings, error) {
	if s.settings == nil || userID == "" {
		return Settings{}, fmt.Errorf("%w: user is required", ErrInvalid)
	}
	if basicStyle != "" {
		if _, err := NormalizeBasicStyle(basicStyle); err != nil {
			return Settings{}, err
		}
	}
	for rawTrait, rawLevel := range extraTraitLevels {
		if _, err := NormalizeTraitKey(rawTrait); err != nil {
			return Settings{}, err
		}
		if _, err := NormalizeLevel(rawLevel); err != nil {
			return Settings{}, err
		}
	}
	settings := NormalizeSettings(Settings{
		UserID:           userID,
		BasicStyle:       basicStyle,
		ExtraTraitLevels: extraTraitLevels,
	})
	if err := s.settings.SavePromptSettings(ctx, appID, settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// ResetSettings 清除用户偏好，之后恢复默认。
func (s *Service) ResetSettings(ctx context.Context, appID, userID string) error {
	if s.settings == nil || userID == "" {
		return fmt.Errorf("%w: user is required", ErrInvalid)
	}
	if err := s.settings.DeletePromptSettings(ctx, appID, userID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}
