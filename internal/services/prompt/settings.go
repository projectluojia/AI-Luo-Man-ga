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

var basicStyleAliases = map[string]string{
	"default": "default", "默认": "default",
	"professional": "professional", "专业可靠": "professional",
	"friendly": "friendly", "亲和友善": "friendly",
	"direct": "direct", "直言不讳": "direct",
	"imaginative": "imaginative", "天马行空": "imaginative",
	"pragmatic": "pragmatic", "高效务实": "pragmatic",
	"roast": "roast", "吐槽达人": "roast",
}

var extraTraitAliases = map[string]string{
	"considerate": "considerate", "温和体贴": "considerate",
	"enthusiastic": "enthusiastic", "热情洋溢": "enthusiastic",
	"emoji": "emoji", "表情符号": "emoji",
	"headings_lists": "headings_lists", "headings": "headings_lists",
	"lists": "headings_lists", "标题和列表": "headings_lists",
}

var levelAliases = map[string]string{
	"enhanced": "enhanced", "strong": "enhanced", "increase": "enhanced", "增强": "enhanced",
	"default": "default", "normal": "default", "默认": "default",
	"reduced": "reduced", "weak": "reduced", "decrease": "reduced", "减弱": "reduced",
}

// NormalizeBasicStyle 把用户输入归一化为目录中的稳定 Key。
func NormalizeBasicStyle(raw string) (string, error) {
	key, ok := basicStyleAliases[strings.ToLower(strings.TrimSpace(raw))]
	if !ok {
		key, ok = basicStyleAliases[strings.TrimSpace(raw)]
	}
	if !ok {
		return "", ErrInvalid
	}
	return key, nil
}

// NormalizeTraitKey 把用户输入归一化为目录中的稳定 Key。
func NormalizeTraitKey(raw string) (string, error) {
	key, ok := extraTraitAliases[strings.ToLower(strings.TrimSpace(raw))]
	if !ok {
		key, ok = extraTraitAliases[strings.TrimSpace(raw)]
	}
	if !ok {
		return "", ErrInvalid
	}
	return key, nil
}

// NormalizeLevel 把用户输入归一化为 enhanced/default/reduced。
func NormalizeLevel(raw string) (string, error) {
	level, ok := levelAliases[strings.ToLower(strings.TrimSpace(raw))]
	if !ok {
		level, ok = levelAliases[strings.TrimSpace(raw)]
	}
	if !ok {
		return "", ErrInvalid
	}
	return level, nil
}

// NormalizeSettings 规范化一份设置：风格未知时回退默认，特征级别只保留已知键。
func NormalizeSettings(settings Settings) Settings {
	settings.UserID = strings.TrimSpace(settings.UserID)
	settings.BasicStyle = strings.TrimSpace(settings.BasicStyle)
	normalizedBasicStyle, err := NormalizeBasicStyle(settings.BasicStyle)
	if err != nil {
		settings.BasicStyle = "default"
	} else {
		settings.BasicStyle = normalizedBasicStyle
	}
	levels := make(map[string]string, len(settings.ExtraTraitLevels))
	for rawTrait, rawLevel := range settings.ExtraTraitLevels {
		trait, traitErr := NormalizeTraitKey(rawTrait)
		level, levelErr := NormalizeLevel(rawLevel)
		if traitErr == nil && levelErr == nil {
			levels[trait] = level
		}
	}
	settings.ExtraTraitLevels = levels
	return settings
}
