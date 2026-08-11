package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Service 是身份与授权域的受治理控制面入口。平台接入只通过 ResolveIdentity
// 提交外部身份；绑定、授权、禁用与撤权全部经过本服务的业务规则。
type Service struct {
	store Store
	now   func() time.Time
}

// NewService 构造身份 Service。store 必须是非 nil 的存储端口实现。
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// CreateUser 创建 Deployment 级内部用户。user_id 由管理控制面显式提供，
// 平台外部标识永远不会被当作内部 user_id。
func (s *Service) CreateUser(ctx context.Context, userID string) (User, error) {
	if err := ValidateUserID(userID); err != nil {
		return User{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	user, err := s.store.CreateUser(ctx, User{UserID: userID, Status: UserStatusActive})
	if err != nil {
		return User{}, err
	}
	observe.Info(ctx, "Deployment 内部用户已创建",
		observe.Component("identity"),
		observe.StringAttr("user_id", userID),
	)
	return user, nil
}

// GetUser 按内部 user_id 读取 Deployment 级用户。
func (s *Service) GetUser(ctx context.Context, userID string) (User, error) {
	if err := ValidateUserID(userID); err != nil {
		return User{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	return s.store.GetUser(ctx, userID)
}

// DisableUser 禁用用户。禁用立即生效：ResolveIdentity 与 EffectivePermissions
// 在下一个查询时刻返回 ErrUserDisabled。
func (s *Service) DisableUser(ctx context.Context, userID string) (User, error) {
	if err := ValidateUserID(userID); err != nil {
		return User{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	user, err := s.store.SetUserStatus(ctx, userID, UserStatusDisabled, s.now().UTC())
	if err != nil {
		return User{}, err
	}
	observe.Info(ctx, "Deployment 内部用户已禁用",
		observe.Component("identity"),
		observe.StringAttr("user_id", userID),
	)
	return user, nil
}

// EnableUser 重新启用用户。
func (s *Service) EnableUser(ctx context.Context, userID string) (User, error) {
	if err := ValidateUserID(userID); err != nil {
		return User{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	user, err := s.store.SetUserStatus(ctx, userID, UserStatusActive, s.now().UTC())
	if err != nil {
		return User{}, err
	}
	observe.Info(ctx, "Deployment 内部用户已启用",
		observe.Component("identity"),
		observe.StringAttr("user_id", userID),
	)
	return user, nil
}

// BindExternalIdentity 把 App 级外部平台身份绑定到已存在的内部用户。
// 目标用户不存在返回 ErrNotFound（不自动创建匿名权威用户）；目标用户已禁用
// 返回 ErrUserDisabled；同一外部身份已绑定到其他用户返回 ErrAlreadyBound。
func (s *Service) BindExternalIdentity(ctx context.Context, binding ExternalIdentity) error {
	normalized, err := NormalizeExternalIdentity(binding)
	if err != nil {
		return fmt.Errorf("%w: platform=%q", ErrInvalid, binding.Platform)
	}
	if err := s.store.BindExternalIdentity(ctx, normalized); err != nil {
		return err
	}
	observe.Info(ctx, "外部平台身份已绑定内部用户",
		observe.Component("identity"),
		observe.StringAttr("app_id", normalized.AppID),
		observe.StringAttr("user_id", normalized.UserID),
		observe.StringAttr("platform", normalized.Platform),
	)
	return nil
}

// UnbindExternalIdentity 解绑外部平台身份。身份不存在返回 ErrNotFound。
func (s *Service) UnbindExternalIdentity(ctx context.Context, appID, platform, platformSpaceID, platformUserID string) error {
	appID, platform, platformSpaceID, platformUserID, err := NormalizeBindingKey(appID, platform, platformSpaceID, platformUserID)
	if err != nil {
		return err
	}
	if err := s.store.UnbindExternalIdentity(ctx, appID, platform, platformSpaceID, platformUserID); err != nil {
		return err
	}
	observe.Info(ctx, "外部平台身份已解绑",
		observe.Component("identity"),
		observe.StringAttr("app_id", appID),
		observe.StringAttr("platform", platform),
	)
	return nil
}

// ResolveIdentity 把平台外部身份解析为受治理身份上下文（查询时计算）。
// 身份不存在返回 ErrNotFound；绑定用户已禁用返回 ErrUserDisabled。
func (s *Service) ResolveIdentity(ctx context.Context, appID, platform, platformSpaceID, platformUserID string) (IdentityContext, error) {
	appID, platform, platformSpaceID, platformUserID, err := NormalizeBindingKey(appID, platform, platformSpaceID, platformUserID)
	if err != nil {
		return IdentityContext{}, err
	}
	binding, err := s.store.GetExternalIdentity(ctx, appID, platform, platformSpaceID, platformUserID)
	if err != nil {
		return IdentityContext{}, err
	}
	user, err := s.store.GetUser(ctx, binding.UserID)
	if err != nil {
		return IdentityContext{}, err
	}
	if user.Status != UserStatusActive {
		return IdentityContext{}, ErrUserDisabled
	}
	return s.identityContextForActiveUser(ctx, appID, user.UserID)
}

// IdentityContextForUser 按内部 user_id 构造受治理身份上下文（不经过平台绑定）。
func (s *Service) IdentityContextForUser(ctx context.Context, appID, userID string) (IdentityContext, error) {
	if err := ValidateAppID(appID); err != nil {
		return IdentityContext{}, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateUserID(userID); err != nil {
		return IdentityContext{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return IdentityContext{}, err
	}
	if user.Status != UserStatusActive {
		return IdentityContext{}, ErrUserDisabled
	}
	return s.identityContextForActiveUser(ctx, appID, userID)
}

// identityContextForActiveUser 组装身份上下文：成员关系、生效权限与绑定修订号
// 全部来自查询时刻的持久化状态。
func (s *Service) identityContextForActiveUser(ctx context.Context, appID, userID string) (IdentityContext, error) {
	membership, err := s.store.GetMembership(ctx, appID, userID)
	membershipMissing := errors.Is(err, ErrNotFound)
	if err != nil && !membershipMissing {
		return IdentityContext{}, err
	}
	permissions, err := s.store.EffectivePermissions(ctx, appID, userID)
	if err != nil {
		return IdentityContext{}, err
	}
	revision, err := s.store.BindingRevision(ctx, appID)
	if err != nil {
		return IdentityContext{}, err
	}
	context := IdentityContext{
		AppID:           appID,
		UserID:          userID,
		Permissions:     permissions,
		BindingRevision: revision,
	}
	if !membershipMissing {
		context.Membership = &membership
		context.RoleIDs = append([]string(nil), membership.RoleIDs...)
	}
	return context, nil
}

// Membership 返回用户在指定 App 的成员关系；用户不存在或无成员关系时返回 ErrNotFound。
func (s *Service) Membership(ctx context.Context, appID, userID string) (AppMembership, error) {
	if err := ValidateAppID(appID); err != nil {
		return AppMembership{}, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateUserID(userID); err != nil {
		return AppMembership{}, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	return s.store.GetMembership(ctx, appID, userID)
}

// MembersByApp 返回指定 App 的成员列表（App 隔离的管理员查询）。
func (s *Service) MembersByApp(ctx context.Context, appID string) ([]AppMembership, error) {
	if err := ValidateAppID(appID); err != nil {
		return nil, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	return s.store.ListMemberships(ctx, appID)
}

// EffectivePermissions 计算用户在 App 的生效权限（直接授予与角色授予的并集）。
// 权限在查询时实时计算，不缓存；用户不存在返回 ErrNotFound，用户已禁用
// 返回 ErrUserDisabled。
func (s *Service) EffectivePermissions(ctx context.Context, appID, userID string) ([]string, error) {
	if err := ValidateAppID(appID); err != nil {
		return nil, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateUserID(userID); err != nil {
		return nil, fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user.Status != UserStatusActive {
		return nil, ErrUserDisabled
	}
	return s.store.EffectivePermissions(ctx, appID, userID)
}

// EnsureRole 创建或更新 App 级角色元数据。角色权限通过 GrantPermission(role subject) 管理。
func (s *Service) EnsureRole(ctx context.Context, role Role) error {
	normalized, err := NormalizeRole(role)
	if err != nil {
		return fmt.Errorf("%w: role_id=%q", ErrInvalid, role.RoleID)
	}
	if err := s.store.EnsureRole(ctx, normalized); err != nil {
		return err
	}
	observe.Info(ctx, "App 角色已写入",
		observe.Component("identity"),
		observe.StringAttr("app_id", normalized.AppID),
		observe.StringAttr("role_id", normalized.RoleID),
	)
	return nil
}

// DeleteRole 删除 App 级角色。角色仍被成员引用时返回 ErrRoleInUse。
func (s *Service) DeleteRole(ctx context.Context, appID, roleID string) error {
	if err := ValidateAppID(appID); err != nil {
		return fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateRoleID(roleID); err != nil {
		return fmt.Errorf("%w: role_id=%q", ErrInvalid, roleID)
	}
	if err := s.store.DeleteRole(ctx, appID, roleID); err != nil {
		return err
	}
	observe.Info(ctx, "App 角色已删除",
		observe.Component("identity"),
		observe.StringAttr("app_id", appID),
		observe.StringAttr("role_id", roleID),
	)
	return nil
}

// GetRole 按 (app_id, role_id) 读取角色。
func (s *Service) GetRole(ctx context.Context, appID, roleID string) (Role, error) {
	if err := ValidateAppID(appID); err != nil {
		return Role{}, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateRoleID(roleID); err != nil {
		return Role{}, fmt.Errorf("%w: role_id=%q", ErrInvalid, roleID)
	}
	return s.store.GetRole(ctx, appID, roleID)
}

// ListRoles 列出 App 内全部角色。
func (s *Service) ListRoles(ctx context.Context, appID string) ([]Role, error) {
	if err := ValidateAppID(appID); err != nil {
		return nil, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	return s.store.ListRoles(ctx, appID)
}

// SetMembership 写入用户在 App 的成员关系（角色集合整体替换）。
// 用户不存在返回 ErrNotFound；引用不存在的角色返回 ErrRoleNotFound。
func (s *Service) SetMembership(ctx context.Context, membership AppMembership) error {
	normalized, err := NormalizeMembership(membership)
	if err != nil {
		return fmt.Errorf("%w: user_id=%q", ErrInvalid, membership.UserID)
	}
	if err := s.store.SetMembership(ctx, normalized); err != nil {
		return err
	}
	observe.Info(ctx, "App 成员关系已写入",
		observe.Component("identity"),
		observe.StringAttr("app_id", normalized.AppID),
		observe.StringAttr("user_id", normalized.UserID),
		observe.IntAttr("role_count", len(normalized.RoleIDs)),
	)
	return nil
}

// RemoveMembership 移除用户在 App 的成员关系。成员的直接权限授予随之外键级联撤销。
func (s *Service) RemoveMembership(ctx context.Context, appID, userID string) error {
	if err := ValidateAppID(appID); err != nil {
		return fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	if err := ValidateUserID(userID); err != nil {
		return fmt.Errorf("%w: user_id=%q", ErrInvalid, userID)
	}
	if err := s.store.RemoveMembership(ctx, appID, userID); err != nil {
		return err
	}
	observe.Info(ctx, "App 成员关系已移除",
		observe.Component("identity"),
		observe.StringAttr("app_id", appID),
		observe.StringAttr("user_id", userID),
	)
	return nil
}

// GrantPermission 授予用户或角色一项权限。直接授予要求成员关系已存在；
// 角色授予要求角色已存在。重复授予幂等成功，不推进修订号。
func (s *Service) GrantPermission(ctx context.Context, grant PermissionGrant) error {
	if err := ValidatePermissionGrant(grant); err != nil {
		return fmt.Errorf("%w: permission=%q", ErrInvalid, grant.Permission)
	}
	if err := s.store.GrantPermission(ctx, grant); err != nil {
		return err
	}
	observe.Info(ctx, "权限已授予",
		observe.Component("identity"),
		observe.StringAttr("app_id", grant.AppID),
		observe.StringAttr("user_id", grant.UserID),
		observe.StringAttr("role_id", grant.RoleID),
		observe.StringAttr("permission", grant.Permission),
	)
	return nil
}

// RevokePermission 撤销用户或角色的一项权限。权限未授予时返回 ErrNotFound。
func (s *Service) RevokePermission(ctx context.Context, grant PermissionGrant) error {
	if err := ValidatePermissionGrant(grant); err != nil {
		return fmt.Errorf("%w: permission=%q", ErrInvalid, grant.Permission)
	}
	if err := s.store.RevokePermission(ctx, grant); err != nil {
		return err
	}
	observe.Info(ctx, "权限已撤销",
		observe.Component("identity"),
		observe.StringAttr("app_id", grant.AppID),
		observe.StringAttr("user_id", grant.UserID),
		observe.StringAttr("role_id", grant.RoleID),
		observe.StringAttr("permission", grant.Permission),
	)
	return nil
}

// BindingRevision 返回 App 当前的绑定修订号；尚无任何身份变更时为 0。
func (s *Service) BindingRevision(ctx context.Context, appID string) (int64, error) {
	if err := ValidateAppID(appID); err != nil {
		return 0, fmt.Errorf("%w: app_id=%q", ErrInvalid, appID)
	}
	return s.store.BindingRevision(ctx, appID)
}
