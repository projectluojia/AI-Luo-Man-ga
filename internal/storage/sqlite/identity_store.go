package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

// 编译期断言：sqlite.Store 必须完整实现 identity.Store 端口。
var _ identity.Store = (*Store)(nil)

// bumpIdentityBindingRevision 在同一事务内把 App 的绑定修订号原子加一。
// 每个 App 级身份/授权变更都必须调用它，保证外部可观察的修订号单调递增。
func bumpIdentityBindingRevision(ctx context.Context, tx *sql.Tx, appID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO identity_binding_revisions(app_id,revision,updated_at) VALUES(?,1,?)
ON CONFLICT(app_id) DO UPDATE SET revision=revision+1,updated_at=excluded.updated_at`,
		appID, now.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("bump identity binding revision: %w", err)
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, user identity.User) (_ identity.User, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_create_user", started, resultErr) }()
	normalized, err := identity.NormalizeUser(user)
	if err != nil {
		return identity.User{}, err
	}
	// 控制面创建的用户一律以启用状态开始，禁用走 SetUserStatus 状态机。
	if normalized.Status != identity.UserStatusActive {
		return identity.User{}, identity.ErrInvalid
	}
	createdAt := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO users(user_id,status,created_at) VALUES(?,?,?)`,
		normalized.UserID, normalized.Status, createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return identity.User{}, identity.ErrConflict
		}
		return identity.User{}, fmt.Errorf("create user: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return identity.User{}, fmt.Errorf("read created user row count: %w", err)
	}
	if affected != 1 {
		return identity.User{}, identity.ErrConflict
	}
	return identity.User{UserID: normalized.UserID, Status: normalized.Status, CreatedAt: createdAt}, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (_ identity.User, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_get_user", started, resultErr) }()
	if err := identity.ValidateUserID(userID); err != nil {
		return identity.User{}, identity.ErrInvalid
	}
	var user identity.User
	var createdAt string
	var disabledAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT user_id,status,created_at,disabled_at FROM users WHERE user_id=?`, userID,
	).Scan(&user.UserID, &user.Status, &createdAt, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.User{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("get user: %w", err)
	}
	var parseErr error
	user.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
	if parseErr != nil {
		return identity.User{}, fmt.Errorf("parse user creation time: %w", parseErr)
	}
	user.DisabledAt, parseErr = parseOptionalTime(disabledAt)
	if parseErr != nil {
		return identity.User{}, fmt.Errorf("parse user disable time: %w", parseErr)
	}
	return user, nil
}

func (s *Store) SetUserStatus(ctx context.Context, userID, status string, at time.Time) (_ identity.User, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_set_user_status", started, resultErr) }()
	if err := identity.ValidateUserID(userID); err != nil {
		return identity.User{}, identity.ErrInvalid
	}
	if (status != identity.UserStatusActive && status != identity.UserStatusDisabled) || at.IsZero() {
		return identity.User{}, identity.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return identity.User{}, fmt.Errorf("begin user status update: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "set user status")
	var currentStatus string
	var createdAt string
	var disabledAt sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT status,created_at,disabled_at FROM users WHERE user_id=?`, userID,
	).Scan(&currentStatus, &createdAt, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.User{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("read user status: %w", err)
	}
	createdAtTime, parseErr := time.Parse(time.RFC3339Nano, createdAt)
	if parseErr != nil {
		return identity.User{}, fmt.Errorf("parse user creation time: %w", parseErr)
	}
	storedDisabledAt, parseErr := parseOptionalTime(disabledAt)
	if parseErr != nil {
		return identity.User{}, fmt.Errorf("parse user disable time: %w", parseErr)
	}
	now := at.UTC()
	if currentStatus == status {
		// 幂等重放：状态未变化时直接返回当前用户（保留已存储的禁用时间），不推进修订号。
		user := identity.User{UserID: userID, Status: currentStatus, CreatedAt: createdAtTime, DisabledAt: storedDisabledAt}
		if err := tx.Commit(); err != nil {
			return identity.User{}, fmt.Errorf("commit unchanged user status: %w", err)
		}
		return user, nil
	}
	var disabledAtValue any
	if status == identity.UserStatusDisabled {
		disabledAtValue = now.Format(time.RFC3339Nano)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET status=?,disabled_at=? WHERE user_id=?`,
		status, disabledAtValue, userID,
	); err != nil {
		return identity.User{}, fmt.Errorf("update user status: %w", err)
	}
	// 用户状态影响其全部 App 的授权，逐个 App 推进绑定修订号。
	// 覆盖范围取成员关系与外部身份绑定的并集：仅有绑定没有成员的 App
	// 也能通过 ResolveIdentity 解析该用户，其身份快照同样必须失效。
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT app_id FROM (
  SELECT app_id FROM app_memberships WHERE user_id=?
  UNION
  SELECT app_id FROM external_identities WHERE user_id=?
) ORDER BY app_id`, userID, userID)
	if err != nil {
		return identity.User{}, fmt.Errorf("query user membership apps: %w", err)
	}
	appIDs := make([]string, 0)
	for rows.Next() {
		var appID string
		if err := rows.Scan(&appID); err != nil {
			rows.Close()
			return identity.User{}, fmt.Errorf("scan user membership app: %w", err)
		}
		appIDs = append(appIDs, appID)
	}
	if err := rows.Close(); err != nil {
		return identity.User{}, fmt.Errorf("close user membership apps: %w", err)
	}
	if err := rows.Err(); err != nil {
		return identity.User{}, fmt.Errorf("iterate user membership apps: %w", err)
	}
	for _, appID := range appIDs {
		if err := bumpIdentityBindingRevision(ctx, tx, appID, now); err != nil {
			return identity.User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return identity.User{}, fmt.Errorf("commit user status update: %w", err)
	}
	user := identity.User{UserID: userID, Status: status, CreatedAt: createdAtTime}
	if status == identity.UserStatusDisabled {
		user.DisabledAt = timePointer(now)
	}
	return user, nil
}

func (s *Store) BindExternalIdentity(ctx context.Context, binding identity.ExternalIdentity) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_bind_external", started, resultErr) }()
	normalized, err := identity.NormalizeExternalIdentity(binding)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin external identity bind: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "bind external identity")
	// 目标内部用户必须已经存在且处于启用状态：平台接入绝不自动创建匿名权威用户。
	var userStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM users WHERE user_id=?`, normalized.UserID).Scan(&userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read bind target user: %w", err)
	}
	if userStatus != identity.UserStatusActive {
		return identity.ErrUserDisabled
	}
	var existingUserID string
	err = tx.QueryRowContext(ctx, `
SELECT user_id FROM external_identities
WHERE app_id=? AND platform=? AND platform_space_id=? AND platform_user_id=?`,
		normalized.AppID, normalized.Platform, normalized.PlatformSpaceID, normalized.PlatformUserID,
	).Scan(&existingUserID)
	switch {
	case err == nil:
		if existingUserID == normalized.UserID {
			// 幂等重放：同一外部身份重复绑定到同一用户视为成功，不推进修订号。
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit idempotent external identity bind: %w", err)
			}
			return nil
		}
		return identity.ErrAlreadyBound
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read existing external identity: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO external_identities(app_id,platform,platform_space_id,platform_user_id,user_id,bound_at)
VALUES(?,?,?,?,?,?)`,
		normalized.AppID, normalized.Platform, normalized.PlatformSpaceID, normalized.PlatformUserID,
		normalized.UserID, now.Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraint(err) {
			// 并发竞争兜底：重新读取后区分相同用户（幂等）与其他用户（冲突）。
			var winner string
			readErr := tx.QueryRowContext(ctx, `
SELECT user_id FROM external_identities
WHERE app_id=? AND platform=? AND platform_space_id=? AND platform_user_id=?`,
				normalized.AppID, normalized.Platform, normalized.PlatformSpaceID, normalized.PlatformUserID,
			).Scan(&winner)
			if readErr == nil && winner == normalized.UserID {
				if err := tx.Commit(); err != nil {
					return fmt.Errorf("commit concurrent idempotent external identity bind: %w", err)
				}
				return nil
			}
			return identity.ErrAlreadyBound
		}
		return fmt.Errorf("insert external identity: %w", err)
	}
	if err := bumpIdentityBindingRevision(ctx, tx, normalized.AppID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit external identity bind: %w", err)
	}
	return nil
}

func (s *Store) UnbindExternalIdentity(ctx context.Context, appID, platform, platformSpaceID, platformUserID string) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_unbind_external", started, resultErr) }()
	if err := identity.ValidateBindingKey(appID, platform, platformSpaceID, platformUserID); err != nil {
		return identity.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin external identity unbind: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "unbind external identity")
	result, err := tx.ExecContext(ctx, `
DELETE FROM external_identities
WHERE app_id=? AND platform=? AND platform_space_id=? AND platform_user_id=?`,
		appID, platform, platformSpaceID, platformUserID,
	)
	if err != nil {
		return fmt.Errorf("delete external identity: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted external identity row count: %w", err)
	}
	if affected != 1 {
		return identity.ErrNotFound
	}
	if err := bumpIdentityBindingRevision(ctx, tx, appID, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit external identity unbind: %w", err)
	}
	return nil
}

func (s *Store) GetExternalIdentity(ctx context.Context, appID, platform, platformSpaceID, platformUserID string) (_ identity.ExternalIdentity, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_get_external", started, resultErr) }()
	if err := identity.ValidateBindingKey(appID, platform, platformSpaceID, platformUserID); err != nil {
		return identity.ExternalIdentity{}, identity.ErrInvalid
	}
	var binding identity.ExternalIdentity
	var boundAt string
	err := s.db.QueryRowContext(ctx, `
SELECT app_id,platform,platform_space_id,platform_user_id,user_id,bound_at
FROM external_identities
WHERE app_id=? AND platform=? AND platform_space_id=? AND platform_user_id=?`,
		appID, platform, platformSpaceID, platformUserID,
	).Scan(&binding.AppID, &binding.Platform, &binding.PlatformSpaceID, &binding.PlatformUserID, &binding.UserID, &boundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ExternalIdentity{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.ExternalIdentity{}, fmt.Errorf("get external identity: %w", err)
	}
	binding.BoundAt, err = time.Parse(time.RFC3339Nano, boundAt)
	if err != nil {
		return identity.ExternalIdentity{}, fmt.Errorf("parse external identity bind time: %w", err)
	}
	return binding, nil
}

func (s *Store) SetMembership(ctx context.Context, membership identity.AppMembership) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_set_membership", started, resultErr) }()
	normalized, err := identity.NormalizeMembership(membership)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin membership ensure: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "set membership")
	// 成员关系必须引用已存在的 Deployment 级用户，绝不自动创建匿名权威用户。
	var userCount int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM users WHERE user_id=?`, normalized.UserID,
	).Scan(&userCount); err != nil {
		return fmt.Errorf("read membership user existence: %w", err)
	}
	if userCount != 1 {
		return identity.ErrNotFound
	}
	for _, roleID := range normalized.RoleIDs {
		var roleCount int
		if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM roles WHERE app_id=? AND role_id=?`,
			normalized.AppID, roleID,
		).Scan(&roleCount); err != nil {
			return fmt.Errorf("read role existence: %w", err)
		}
		if roleCount != 1 {
			return fmt.Errorf("%w: role_id=%q", identity.ErrRoleNotFound, roleID)
		}
	}
	now := time.Now().UTC()
	roleIDsJSON, err := json.Marshal(normalized.RoleIDs)
	if err != nil {
		return errors.Join(identity.ErrInvalid, err)
	}
	// 幂等重放：角色集合未变化时不推进修订号。
	var existingRoleIDs string
	err = tx.QueryRowContext(ctx, `
SELECT role_ids FROM app_memberships WHERE app_id=? AND user_id=?`,
		normalized.AppID, normalized.UserID,
	).Scan(&existingRoleIDs)
	alreadyIdentical := err == nil && existingRoleIDs == string(roleIDsJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing membership: %w", err)
	}
	if !alreadyIdentical {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO app_memberships(app_id,user_id,role_ids,created_at,updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(app_id,user_id) DO UPDATE SET role_ids=excluded.role_ids,updated_at=excluded.updated_at`,
			normalized.AppID, normalized.UserID, string(roleIDsJSON),
			now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("upsert membership: %w", err)
		}
		if err := bumpIdentityBindingRevision(ctx, tx, normalized.AppID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit membership ensure: %w", err)
	}
	return nil
}

func (s *Store) GetMembership(ctx context.Context, appID, userID string) (_ identity.AppMembership, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_get_membership", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil || identity.ValidateUserID(userID) != nil {
		return identity.AppMembership{}, identity.ErrInvalid
	}
	var membership identity.AppMembership
	var roleIDsJSON string
	var createdAt string
	var updatedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT app_id,user_id,role_ids,created_at,updated_at
FROM app_memberships WHERE app_id=? AND user_id=?`, appID, userID,
	).Scan(&membership.AppID, &membership.UserID, &roleIDsJSON, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.AppMembership{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.AppMembership{}, fmt.Errorf("get membership: %w", err)
	}
	if err := json.Unmarshal([]byte(roleIDsJSON), &membership.RoleIDs); err != nil {
		return identity.AppMembership{}, errors.Join(identity.ErrInvalid, err)
	}
	var parseErr error
	membership.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
	if parseErr != nil {
		return identity.AppMembership{}, fmt.Errorf("parse membership creation time: %w", parseErr)
	}
	membership.UpdatedAt, parseErr = time.Parse(time.RFC3339Nano, updatedAt)
	if parseErr != nil {
		return identity.AppMembership{}, fmt.Errorf("parse membership update time: %w", parseErr)
	}
	return membership, nil
}

// EffectivePermissions 在查询时刻实时计算用户在 App 的生效权限
// （成员直接授予与角色授予的并集），不缓存，保证撤权立即生效。
func (s *Store) EffectivePermissions(ctx context.Context, appID, userID string) (_ []string, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_effective_permissions", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil || identity.ValidateUserID(userID) != nil {
		return nil, identity.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT permission FROM (
  SELECT permission FROM permission_grants
  WHERE app_id=? AND user_id=? AND user_id IS NOT NULL
  UNION ALL
  SELECT pg.permission FROM permission_grants pg
  JOIN app_memberships m ON m.app_id=pg.app_id AND m.user_id=?
  JOIN json_each(m.role_ids) role ON role.value=pg.role_id
  WHERE pg.app_id=? AND pg.role_id IS NOT NULL
) ORDER BY permission`,
		appID, userID, userID, appID,
	)
	if err != nil {
		return nil, fmt.Errorf("query effective permissions: %w", err)
	}
	defer rows.Close()
	permissions := make([]string, 0)
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return nil, fmt.Errorf("scan effective permission: %w", err)
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective permissions: %w", err)
	}
	return permissions, nil
}

func (s *Store) BindingRevision(ctx context.Context, appID string) (_ int64, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "identity_binding_revision", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return 0, identity.ErrInvalid
	}
	var revision int64
	err := s.db.QueryRowContext(ctx, `
SELECT revision FROM identity_binding_revisions WHERE app_id=?`, appID,
	).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		// 尚无任何身份变更时修订号为 0，这是合法的初始状态。
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read binding revision: %w", err)
	}
	return revision, nil
}
