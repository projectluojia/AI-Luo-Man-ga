// Package settings 定义 QQ Access 自己的持久配置契约。
package settings

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

const MaxAllowlistEntries = 1024

var (
	ErrNotFound = errors.New("qq access settings not found")
	ErrConflict = errors.New("qq access settings generation conflict")
	ErrInvalid  = errors.New("invalid qq access settings")
)

// Settings 是 AI珞侧 QQ 接入配置。NapCat 的登录与 OneBot 服务配置不属于本结构。
type Settings struct {
	AppID                 string    `json:"app_id"`
	Enabled               bool      `json:"enabled"`
	WSURL                 string    `json:"ws_url"`
	BotQQID               string    `json:"bot_qq_id"`
	AllowedGroupIDs       []string  `json:"allowed_group_ids"`
	AllowedPrivateUserIDs []string  `json:"allowed_private_user_ids"`
	Generation            uint64    `json:"generation"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// Store 是 QQ Access 配置所需的窄持久化端口。
type Store interface {
	EnsureQQSettings(context.Context, Settings) (Settings, bool, error)
	CurrentQQSettings(context.Context, string) (Settings, error)
	CompareAndSwapQQSettings(context.Context, uint64, Settings) (Settings, error)
}

// Normalize 规范化白名单顺序并校验全部边界。
func Normalize(value Settings) (Settings, error) {
	value.AppID = strings.TrimSpace(value.AppID)
	value.WSURL = strings.TrimSpace(value.WSURL)
	if identity.ValidateAppID(value.AppID) != nil {
		return Settings{}, ErrInvalid
	}
	if value.Enabled && (value.WSURL == "" || value.BotQQID == "") {
		return Settings{}, ErrInvalid
	}
	if len(value.WSURL) > 2048 || value.WSURL != "" && !validWSURL(value.WSURL) {
		return Settings{}, ErrInvalid
	}
	if value.BotQQID != "" {
		if _, valid := NormalizeQQID(value.BotQQID); !valid {
			return Settings{}, ErrInvalid
		}
	}
	groups, err := normalizeAllowlist(value.AllowedGroupIDs)
	if err != nil {
		return Settings{}, err
	}
	privateUsers, err := normalizeAllowlist(value.AllowedPrivateUserIDs)
	if err != nil {
		return Settings{}, err
	}
	value.AllowedGroupIDs = groups
	value.AllowedPrivateUserIDs = privateUsers
	return value, nil
}

// EqualContent 比较影响适配器行为的配置，不比较代数和更新时间。
func EqualContent(left, right Settings) bool {
	return left.AppID == right.AppID && left.Enabled == right.Enabled && left.WSURL == right.WSURL &&
		left.BotQQID == right.BotQQID && slices.Equal(left.AllowedGroupIDs, right.AllowedGroupIDs) &&
		slices.Equal(left.AllowedPrivateUserIDs, right.AllowedPrivateUserIDs)
}

// NormalizeQQID 接受规范十进制正整数 QQ 标识。
func NormalizeQQID(value string) (string, bool) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", false
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil || number == 0 {
		return "", false
	}
	normalized := strconv.FormatUint(number, 10)
	return normalized, normalized == value
}

func normalizeAllowlist(values []string) ([]string, error) {
	if len(values) > MaxAllowlistEntries {
		return nil, ErrInvalid
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized, valid := NormalizeQQID(value)
		if !valid {
			return nil, ErrInvalid
		}
		unique[normalized] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validWSURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "ws" || parsed.Scheme == "wss"
}
