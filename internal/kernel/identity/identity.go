// Package identity 实现 AI珞（爱珞）内核的身份与授权域。
//
// 职责边界：
//   - User 是 Deployment 级内部用户，由管理控制面显式创建，外部平台标识永远
//     不能替代内部 user_id；
//   - ExternalIdentity 是 App 级外部平台身份到内部用户的绑定，唯一键为
//     (app_id, platform, platform_space_id, platform_user_id)，同一外部身份
//     在同一 App 内只能绑定一个内部用户；
//   - AppMembership、Role 与 PermissionGrant 共同表达用户在 App 内的授权，
//     全部按 app_id 隔离，跨 App 读取统一按不存在处理；
//   - IdentityBindingRevision 是 App 级单调递增的身份/授权变更修订号；
//   - 生效权限在查询时实时计算，不缓存；撤权、禁用与解绑立即反映到下一次
//     权限快照（IdentityContext / EffectivePermissions）；
//   - 身份不存在时返回明确的 ErrNotFound，绝不自动创建匿名权威用户。
package identity

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
	ErrRoleInUse    = errors.New("role is still referenced by app members")
)

var (
	stableIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	appIDPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	platformPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	roleIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._:-][a-z0-9]+)*$`)
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
// 角色通过 RoleIDs 引用同 App 的 Role；直接权限通过 PermissionGrant（user subject）表达。
type AppMembership struct {
	AppID     string
	UserID    string
	RoleIDs   []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Role 是 App 级角色定义，唯一键为 (app_id, role_id)。
// 角色的权限通过 PermissionGrant（role subject）表达。
type Role struct {
	AppID       string
	RoleID      string
	Name        string
	Description string
	CreatedAt   time.Time
}

// PermissionGrant 是一条权限授予记录。subject 必须且只能是内部用户（UserID）
// 或 App 角色（RoleID）之一。
type PermissionGrant struct {
	AppID      string
	UserID     string
	RoleID     string
	Permission string
	GrantedAt  time.Time
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

	EnsureRole(context.Context, Role) error
	DeleteRole(context.Context, string, string) error
	GetRole(context.Context, string, string) (Role, error)
	ListRoles(context.Context, string) ([]Role, error)

	SetMembership(context.Context, AppMembership) error
	RemoveMembership(context.Context, string, string) error
	GetMembership(context.Context, string, string) (AppMembership, error)
	ListMemberships(context.Context, string) ([]AppMembership, error)

	GrantPermission(context.Context, PermissionGrant) error
	RevokePermission(context.Context, PermissionGrant) error

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
	if !platformPattern.MatchString(platform) || len(platform) > 128 {
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
	if !roleIDPattern.MatchString(roleID) || len(roleID) > 128 {
		return ErrInvalid
	}
	return nil
}

func ValidatePermission(permission string) error {
	if !permissionPattern.MatchString(permission) || len(permission) > 128 {
		return ErrInvalid
	}
	return nil
}

func ValidateRoleName(name string) error {
	if len(name) == 0 || len(name) > 256 || !utf8.ValidString(name) {
		return ErrInvalid
	}
	return nil
}

func ValidateRoleDescription(description string) error {
	if len(description) > 1024 || !utf8.ValidString(description) {
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

func ValidateRole(role Role) error {
	if err := ValidateAppID(role.AppID); err != nil {
		return err
	}
	if err := ValidateRoleID(role.RoleID); err != nil {
		return err
	}
	if err := ValidateRoleName(role.Name); err != nil {
		return err
	}
	return ValidateRoleDescription(role.Description)
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

func ValidatePermissionGrant(grant PermissionGrant) error {
	if err := ValidateAppID(grant.AppID); err != nil {
		return err
	}
	if (grant.UserID == "") == (grant.RoleID == "") {
		return ErrInvalid
	}
	if grant.UserID != "" {
		if err := ValidateUserID(grant.UserID); err != nil {
			return err
		}
	}
	if grant.RoleID != "" {
		if err := ValidateRoleID(grant.RoleID); err != nil {
			return err
		}
	}
	return ValidatePermission(grant.Permission)
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

func NormalizeRole(role Role) (Role, error) {
	role.AppID = strings.TrimSpace(role.AppID)
	role.RoleID = strings.TrimSpace(role.RoleID)
	role.Name = strings.TrimSpace(role.Name)
	if err := ValidateRole(role); err != nil {
		return Role{}, err
	}
	return role, nil
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
