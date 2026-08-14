package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Service 是身份域的受治理控制面入口：管理控制面经 CreateUser/BindExternalIdentity/
// SetMembership/UnbindExternalIdentity/DisableUser 开通与维护身份，平台接入只通过
// ResolveIdentity 提交外部身份并取回受治理身份快照。
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

// DisableUser 禁用用户。禁用立即生效：ResolveIdentity 在下一个查询时刻
// 返回 ErrUserDisabled（映射为公共错误 403 user_disabled）。
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
