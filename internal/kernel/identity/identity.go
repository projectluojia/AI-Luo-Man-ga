// Package identity 实现 AI珞（爱珞）内核的身份与授权域。
//
// 职责边界：
//   - User 是 Deployment 级内部用户，由管理控制面显式创建，外部平台标识永远
//     不能替代内部 user_id；
//   - ExternalIdentity 是 App 级外部平台身份到内部用户的绑定，唯一键为
//     (app_id, platform, platform_space_id, platform_user_id)，同一外部身份
//     在同一 App 内只能绑定一个内部用户；
//   - AppMembership 是用户在 App 内的成员关系，全部按 app_id 隔离，跨 App
//     读取统一按不存在处理；roles/permission_grants 表保留为 Schema 契约，
//     SetMembership 内联校验角色存在性；
//   - IdentityBindingRevision 是 App 级单调递增的身份/授权变更修订号；
//   - 生效权限在查询时实时计算，不缓存；禁用与解绑立即反映到下一次
//     身份快照（IdentityContext / EffectivePermissions）；
//   - 身份不存在时返回明确的 ErrNotFound，绝不自动创建匿名权威用户。
package identity

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

// 用户状态。
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// 允许出现在单个成员关系中的最大角色数量。
const MaxRoleIDsPerMembership = 32

var (
	ErrNotFound     = errors.New("identity not found")
	ErrInvalid      = errors.New("invalid identity input")
	ErrConflict     = errors.New("identity already exists")
	ErrAlreadyBound = errors.New("external identity is already bound to another user")
	ErrUserDisabled = errors.New("user is disabled")
	ErrRoleNotFound = errors.New("role is not found")
)

var (
	appIDPattern    = id.AppID
	stableIDPattern = id.StableMixed
)

// User 是 Deployment 级内部用户。user_id 与任何外部平台标识都是不同的命名空间。
type User struct {
	UserID     string
	Status     string
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// ExternalIdentity 是 App 级外部平台身份到内部用户的绑定。
// 唯一键为 (app_id, platform, platform_space_id, platform_user_id)。
type ExternalIdentity struct {
	AppID           string
	Platform        string
	PlatformSpaceID string
	PlatformUserID  string
	UserID          string
	BoundAt         time.Time
}

// AppMembership 是用户在指定 App 内的成员关系，唯一键为 (app_id, user_id)。
// 角色通过 RoleIDs 引用同 App 的 Role（角色存在性由 SetMembership 内联校验）。
type AppMembership struct {
	AppID     string
	UserID    string
	RoleIDs   []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IdentityContext 是接入事件解析后的受治理身份快照，供 Dispatcher 与知识库
// 在查询时读取。权限与修订号来自查询时刻的持久化状态，不缓存。
type IdentityContext struct {
	AppID           string
	UserID          string
	Membership      *AppMembership // 用户在 App 无成员关系时为 nil
	RoleIDs         []string
	Permissions     []string // 生效权限（直接授予与角色授予的并集，规范排序）
	BindingRevision int64
}

// Store 是身份与授权域的存储端口。实现必须保证：
//   - 全部 App 级查询同时约束 app_id；
//   - 外部身份唯一键与成员唯一键由数据库唯一约束强制；
//   - 每个 App 级治理变更在同一事务内原子推进 IdentityBindingRevision；
//   - 所有 SQL 参数化，不使用动态拼接标识符。
type Store interface {
	CreateUser(context.Context, User) (User, error)
	GetUser(context.Context, string) (User, error)
	SetUserStatus(context.Context, string, string, time.Time) (User, error)

	BindExternalIdentity(context.Context, ExternalIdentity) error
	UnbindExternalIdentity(context.Context, string, string, string, string) error
	GetExternalIdentity(context.Context, string, string, string, string) (ExternalIdentity, error)

	SetMembership(context.Context, AppMembership) error
	GetMembership(context.Context, string, string) (AppMembership, error)

	EffectivePermissions(context.Context, string, string) ([]string, error)
	BindingRevision(context.Context, string) (int64, error)
}

// 单个字段校验。

func ValidateAppID(appID string) error {
	if !appIDPattern.MatchString(appID) || len(appID) > 128 {
		return ErrInvalid
	}
	return nil
}

func ValidateUserID(userID string) error {
	if !stableIDPattern.MatchString(userID) || len(userID) > 128 {
		return ErrInvalid
	}
	return nil
}

func ValidatePlatform(platform string) error {
	if !capability.IsStableID(platform) {
		return ErrInvalid
	}
	return nil
}

func ValidatePlatformSpaceID(spaceID string) error {
	if !stableIDPattern.MatchString(spaceID) || len(spaceID) > 128 {
		return ErrInvalid
	}
	return nil
}

// ValidatePlatformUserID 校验外部平台侧不透明标识。该值是平台返回的原始字符串，
// 不裁剪空白，只拒绝空值、超长值、非法 UTF-8 与控制字符。
func ValidatePlatformUserID(value string) error {
	if len(value) == 0 || len(value) > 256 || !utf8.ValidString(value) {
		return ErrInvalid
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return ErrInvalid
		}
	}
	return nil
}

func ValidateRoleID(roleID string) error {
	if !capability.IsStableID(roleID) {
		return ErrInvalid
	}
	return nil
}

// ValidateBindingKey 校验外部身份的唯一键字段。
func ValidateBindingKey(appID, platform, platformSpaceID, platformUserID string) error {
	if err := ValidateAppID(appID); err != nil {
		return err
	}
	if err := ValidatePlatform(platform); err != nil {
		return err
	}
	if err := ValidatePlatformSpaceID(platformSpaceID); err != nil {
		return err
	}
	if err := ValidatePlatformUserID(platformUserID); err != nil {
		return err
	}
	return nil
}

// 整体模型校验（要求规范形式）。

func ValidateUser(user User) error {
	if err := ValidateUserID(user.UserID); err != nil {
		return err
	}
	if user.Status != UserStatusActive && user.Status != UserStatusDisabled {
		return ErrInvalid
	}
	if (user.Status == UserStatusActive && user.DisabledAt != nil) ||
		(user.Status == UserStatusDisabled && user.DisabledAt == nil) {
		return ErrInvalid
	}
	return nil
}

func ValidateExternalIdentity(binding ExternalIdentity) error {
	if err := ValidateBindingKey(binding.AppID, binding.Platform, binding.PlatformSpaceID, binding.PlatformUserID); err != nil {
		return err
	}
	return ValidateUserID(binding.UserID)
}

func ValidateAppMembership(membership AppMembership) error {
	if err := ValidateAppID(membership.AppID); err != nil {
		return err
	}
	if err := ValidateUserID(membership.UserID); err != nil {
		return err
	}
	if len(membership.RoleIDs) > MaxRoleIDsPerMembership {
		return ErrInvalid
	}
	for _, roleID := range membership.RoleIDs {
		if err := ValidateRoleID(roleID); err != nil {
			return err
		}
	}
	if !sortedUnique(membership.RoleIDs) {
		return ErrInvalid
	}
	return nil
}

// 规范化：裁剪空白并把集合整理为规范排序、去重形式，之后再做严格校验。

func NormalizeUser(user User) (User, error) {
	user.UserID = strings.TrimSpace(user.UserID)
	if err := ValidateUser(user); err != nil {
		return User{}, err
	}
	return user, nil
}

func NormalizeExternalIdentity(binding ExternalIdentity) (ExternalIdentity, error) {
	binding.AppID = strings.TrimSpace(binding.AppID)
	binding.Platform = strings.TrimSpace(binding.Platform)
	binding.PlatformSpaceID = strings.TrimSpace(binding.PlatformSpaceID)
	binding.UserID = strings.TrimSpace(binding.UserID)
	if err := ValidateExternalIdentity(binding); err != nil {
		return ExternalIdentity{}, err
	}
	return binding, nil
}

func NormalizeBindingKey(appID, platform, platformSpaceID, platformUserID string) (string, string, string, string, error) {
	appID = strings.TrimSpace(appID)
	platform = strings.TrimSpace(platform)
	platformSpaceID = strings.TrimSpace(platformSpaceID)
	if err := ValidateBindingKey(appID, platform, platformSpaceID, platformUserID); err != nil {
		return "", "", "", "", err
	}
	return appID, platform, platformSpaceID, platformUserID, nil
}

func NormalizeMembership(membership AppMembership) (AppMembership, error) {
	membership.AppID = strings.TrimSpace(membership.AppID)
	membership.UserID = strings.TrimSpace(membership.UserID)
	membership.RoleIDs = canonicalStrings(membership.RoleIDs)
	if err := ValidateAppMembership(membership); err != nil {
		return AppMembership{}, err
	}
	return membership, nil
}

func canonicalStrings(values []string) []string {
	// make 而非 append(nil, ...)：空输入保持非 nil 空切片，JSON 序列化为 [] 而非 null。
	result := make([]string, len(values))
	copy(result, values)
	sort.Strings(result)
	return result
}

func sortedUnique(values []string) bool {
	for index, value := range values {
		if value == "" {
			return false
		}
		if index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}
