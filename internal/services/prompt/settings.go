package prompt

import (
	"errors"
	"strings"
)

var ErrInvalid = errors.New("invalid prompt preference")

// Settings 是用户提示词偏好。键使用目录中的稳定 Key，显示名在渲染时从目录取得。
type Settings struct {
	UserID           string
	BasicStyle       string
	ExtraTraitLevels map[string]string
}

// DefaultSettings 返回用户默认偏好。
func DefaultSettings(userID string) Settings {
	return Settings{
		UserID:           userID,
		BasicStyle:       "default",
		ExtraTraitLevels: map[string]string{},
	}
}

// NormalizeBasicStyle 校验目录中的稳定 Key。
func NormalizeBasicStyle(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	switch key {
	case "default", "professional", "friendly", "direct", "imaginative", "pragmatic", "roast":
		return key, nil
	default:
		return "", ErrInvalid
	}
}

// NormalizeTraitKey 校验目录中的稳定 Key。
func NormalizeTraitKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	switch key {
	case "considerate", "enthusiastic", "emoji", "headings_lists":
		return key, nil
	default:
		return "", ErrInvalid
	}
}

// NormalizeLevel 校验 enhanced/default/reduced。
func NormalizeLevel(raw string) (string, error) {
	level := strings.TrimSpace(raw)
	switch level {
	case "enhanced", "default", "reduced":
		return level, nil
	default:
		return "", ErrInvalid
	}
}

// NormalizeSettings 校验并规范化一份设置。
func NormalizeSettings(settings Settings) (Settings, error) {
	settings.UserID = strings.TrimSpace(settings.UserID)
	settings.BasicStyle = strings.TrimSpace(settings.BasicStyle)
	if _, err := NormalizeBasicStyle(settings.BasicStyle); err != nil {
		return Settings{}, err
	}
	levels := make(map[string]string, len(settings.ExtraTraitLevels))
	for rawTrait, rawLevel := range settings.ExtraTraitLevels {
		trait, traitErr := NormalizeTraitKey(rawTrait)
		level, levelErr := NormalizeLevel(rawLevel)
		if traitErr != nil {
			return Settings{}, traitErr
		}
		if levelErr != nil {
			return Settings{}, levelErr
		}
		levels[trait] = level
	}
	settings.ExtraTraitLevels = levels
	return settings, nil
}
