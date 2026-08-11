package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func newTestService(t *testing.T) (*identity.Service, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "identity-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return identity.NewService(store), store
}

func mustBind(t *testing.T, service *identity.Service, appID, platform, spaceID, platformUserID, userID string) {
	t.Helper()
	if err := service.BindExternalIdentity(context.Background(), identity.ExternalIdentity{
		AppID: appID, Platform: platform, PlatformSpaceID: spaceID, PlatformUserID: platformUserID, UserID: userID,
	}); err != nil {
		t.Fatalf("bind %s/%s/%s -> %s: %v", platform, spaceID, platformUserID, userID, err)
	}
}

func mustMembership(t *testing.T, service *identity.Service, appID, userID string, roleIDs ...string) {
	t.Helper()
	if err := service.SetMembership(context.Background(), identity.AppMembership{
		AppID: appID, UserID: userID, RoleIDs: roleIDs,
	}); err != nil {
		t.Fatalf("set membership %s/%s: %v", appID, userID, err)
	}
}

func mustRole(t *testing.T, service *identity.Service, appID, roleID, name string) {
	t.Helper()
	if err := service.EnsureRole(context.Background(), identity.Role{AppID: appID, RoleID: roleID, Name: name}); err != nil {
		t.Fatalf("ensure role %s/%s: %v", appID, roleID, err)
	}
}

func mustGrantUser(t *testing.T, service *identity.Service, appID, userID, permission string) {
	t.Helper()
	if err := service.GrantPermission(context.Background(), identity.PermissionGrant{
		AppID: appID, UserID: userID, Permission: permission,
	}); err != nil {
		t.Fatalf("grant %s/%s: %v", userID, permission, err)
	}
}

func mustGrantRole(t *testing.T, service *identity.Service, appID, roleID, permission string) {
	t.Helper()
	if err := service.GrantPermission(context.Background(), identity.PermissionGrant{
		AppID: appID, RoleID: roleID, Permission: permission,
	}); err != nil {
		t.Fatalf("grant role %s/%s: %v", roleID, permission, err)
	}
}

func sortedEqual(actual, expected []string) bool {
	sort.Strings(actual)
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

// 验收标准 1：外部平台 ID 不能直接充当内部 user_id。
func TestExternalPlatformIDNeverBecomesInternalUserID(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustBind(t, service, "app-a", "qq", "space-1", "external-alice", "user-1")

	resolved, err := service.ResolveIdentity(ctx, "app-a", "qq", "space-1", "external-alice")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.UserID != "user-1" {
		t.Fatalf("resolved user_id=%q, want internal user-1", resolved.UserID)
	}
	// 平台标识从未被当作内部用户：以平台 ID 查询用户与权限都必须明确不存在。
	if _, err := service.GetUser(ctx, "external-alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("platform ID became a user: %v", err)
	}
	if _, err := service.EffectivePermissions(ctx, "app-a", "external-alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("platform ID resolved as internal user: %v", err)
	}
	if _, err := service.Membership(ctx, "app-a", "external-alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("platform ID resolved as internal member: %v", err)
	}
}

// 验收标准 5：身份不存在时返回明确状态，不自动创建匿名权威用户。
func TestMissingIdentityReturnsExplicitStatusWithoutAutoCreate(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	// 未绑定身份解析：ErrNotFound，且不会产生任何用户。
	if _, err := service.ResolveIdentity(ctx, "app", "qq", "space", "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("resolve unbound identity error=%v, want ErrNotFound", err)
	}
	if _, err := service.GetUser(ctx, "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("anonymous user was auto-created: %v", err)
	}
	// 绑定到不存在的内部用户：ErrNotFound。
	err := service.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app", Platform: "wechat", PlatformSpaceID: "space", PlatformUserID: "wx-1", UserID: "ghost-user",
	})
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("bind to missing user error=%v, want ErrNotFound", err)
	}
	if _, err := service.GetUser(ctx, "ghost-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("binding auto-created an anonymous user: %v", err)
	}
	// 解绑不存在的身份：ErrNotFound。
	if err := service.UnbindExternalIdentity(ctx, "app", "qq", "space", "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unbind missing identity error=%v, want ErrNotFound", err)
	}
}

// 验收标准 2：同一外部身份不能被绑定给两个用户；同用户重复绑定幂等成功。
func TestSameExternalIdentityCannotBindTwoUsers(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateUser(ctx, "user-b"); err != nil {
		t.Fatal(err)
	}
	mustBind(t, service, "app", "qq", "space", "platform-user", "user-a")

	if err := service.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "platform-user", UserID: "user-b",
	}); !errors.Is(err, identity.ErrAlreadyBound) {
		t.Fatalf("second bind error=%v, want ErrAlreadyBound", err)
	}
	// 幂等重绑到同一用户成功，不推进修订号。
	revisionBefore, err := service.BindingRevision(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	mustBind(t, service, "app", "qq", "space", "platform-user", "user-a")
	revisionAfter, err := service.BindingRevision(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("idempotent rebind advanced revision %d -> %d", revisionBefore, revisionAfter)
	}
	resolved, err := service.ResolveIdentity(ctx, "app", "qq", "space", "platform-user")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.UserID != "user-a" {
		t.Fatalf("binding moved to user %q, want user-a", resolved.UserID)
	}
}

// 验收标准 3：用户在 App A 的权限不能进入 App B；跨 App 解析/读取一律不存在。
func TestPermissionsNeverLeakAcrossApps(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustRole(t, service, "app-a", "editor", "编辑")
	mustMembership(t, service, "app-a", "user-1", "editor")
	mustGrantRole(t, service, "app-a", "editor", "doc.write")
	mustGrantUser(t, service, "app-a", "user-1", "bus.read")
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")

	permissionsA, err := service.EffectivePermissions(ctx, "app-a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sortedEqual(permissionsA, []string{"bus.read", "doc.write"}) {
		t.Fatalf("app-a permissions=%v, want [bus.read doc.write]", permissionsA)
	}
	// 同一用户在 App B 没有任何权限。
	permissionsB, err := service.EffectivePermissions(ctx, "app-b", "user-1")
	if err != nil || len(permissionsB) != 0 {
		t.Fatalf("app-b permissions=%v err=%v, want empty", permissionsB, err)
	}
	if _, err := service.Membership(ctx, "app-b", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("app-b membership should be absent: %v", err)
	}
	if members, err := service.MembersByApp(ctx, "app-b"); err != nil || len(members) != 0 {
		t.Fatalf("app-b member list=%#v err=%v, want empty", members, err)
	}
	// App 级绑定隔离：身份在 app-a 绑定，在 app-b 解析必须明确不存在。
	if _, err := service.ResolveIdentity(ctx, "app-b", "qq", "space", "alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("cross-app resolve error=%v, want ErrNotFound", err)
	}
	bindings, err := store.ListExternalIdentities(ctx, "app-a", "user-1")
	if err != nil || len(bindings) != 1 || bindings[0].AppID != "app-a" {
		t.Fatalf("app-a bindings=%#v err=%v", bindings, err)
	}
	if bindings, err := store.ListExternalIdentities(ctx, "app-b", "user-1"); err != nil || len(bindings) != 0 {
		t.Fatalf("app-b bindings leaked=%#v err=%v", bindings, err)
	}
	// 用户在 App B 重新成为成员后也只拥有 App B 自己的权限。
	mustRole(t, service, "app-b", "viewer", "查看")
	mustMembership(t, service, "app-b", "user-1", "viewer")
	mustGrantRole(t, service, "app-b", "viewer", "bus.read")
	permissionsB, err = service.EffectivePermissions(ctx, "app-b", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sortedEqual(permissionsB, []string{"bus.read"}) {
		t.Fatalf("app-b permissions=%v, want [bus.read]", permissionsB)
	}
	permissionsA, err = service.EffectivePermissions(ctx, "app-a", "user-1")
	if err != nil || !sortedEqual(permissionsA, []string{"bus.read", "doc.write"}) {
		t.Fatalf("app-a permissions changed by app-b grant: %v err=%v", permissionsA, err)
	}
}

// 验收标准 4：撤权/禁用在下一次查询时刻立即生效（权限快照实时计算，不缓存）。
func TestRevocationAndDisableTakeEffectImmediately(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustRole(t, service, "app-a", "member", "成员")
	mustMembership(t, service, "app-a", "user-1", "member")
	mustGrantRole(t, service, "app-a", "member", "bus.read")
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")

	checkPermissions := func(want []string) {
		t.Helper()
		permissions, err := service.EffectivePermissions(ctx, "app-a", "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if !sortedEqual(permissions, want) {
			t.Fatalf("permissions=%v, want %v", permissions, want)
		}
	}
	checkPermissions([]string{"bus.read", "bus.write"})

	// 直接撤权立即生效。
	if err := service.RevokePermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}
	checkPermissions([]string{"bus.read"})

	// 角色撤权立即生效。
	if err := service.RevokePermission(ctx, identity.PermissionGrant{AppID: "app-a", RoleID: "member", Permission: "bus.read"}); err != nil {
		t.Fatal(err)
	}
	checkPermissions(nil)

	// 再次授予后权限恢复。
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")
	mustGrantRole(t, service, "app-a", "member", "bus.read")
	checkPermissions([]string{"bus.read", "bus.write"})

	// 禁用立即生效：解析与权限查询都返回 ErrUserDisabled。
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	if _, err := service.DisableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EffectivePermissions(ctx, "app-a", "user-1"); !errors.Is(err, identity.ErrUserDisabled) {
		t.Fatalf("disabled EffectivePermissions error=%v, want ErrUserDisabled", err)
	}
	// 已存在绑定在禁用后立即拒绝解析。
	if _, err := service.ResolveIdentity(ctx, "app-a", "qq", "space", "alice"); !errors.Is(err, identity.ErrUserDisabled) {
		t.Fatalf("resolve disabled user error=%v, want ErrUserDisabled", err)
	}
	// 已禁用用户禁止新增绑定。
	if err := service.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-a", Platform: "wechat", PlatformSpaceID: "space", PlatformUserID: "wx-1", UserID: "user-1",
	}); !errors.Is(err, identity.ErrUserDisabled) {
		t.Fatalf("bind to disabled user error=%v, want ErrUserDisabled", err)
	}

	// 重新启用后立即恢复。
	if _, err := service.EnableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveIdentity(ctx, "app-a", "qq", "space", "alice")
	if err != nil || resolved.UserID != "user-1" {
		t.Fatalf("resolved after enable=%#v err=%v", resolved, err)
	}
	checkPermissions([]string{"bus.read", "bus.write"})
}

// IdentityContext 至少包含 UserID、AppID、成员、生效权限与绑定修订号。
func TestIdentityContextCarriesMembershipRolesPermissionsAndRevision(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustRole(t, service, "app-a", "member", "成员")
	mustMembership(t, service, "app-a", "user-1", "member")
	mustGrantRole(t, service, "app-a", "member", "bus.read")
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")

	resolved, err := service.ResolveIdentity(ctx, "app-a", "qq", "space", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AppID != "app-a" || resolved.UserID != "user-1" {
		t.Fatalf("context identity=%s/%s, want app-a/user-1", resolved.AppID, resolved.UserID)
	}
	if resolved.Membership == nil || resolved.Membership.AppID != "app-a" || resolved.Membership.UserID != "user-1" {
		t.Fatalf("context membership=%#v", resolved.Membership)
	}
	if !sortedEqual(resolved.RoleIDs, []string{"member"}) {
		t.Fatalf("context role_ids=%v", resolved.RoleIDs)
	}
	if !sortedEqual(resolved.Permissions, []string{"bus.read", "bus.write"}) {
		t.Fatalf("context permissions=%v", resolved.Permissions)
	}
	if resolved.BindingRevision <= 0 {
		t.Fatalf("binding revision=%d, want > 0", resolved.BindingRevision)
	}

	// 无成员关系的用户：身份仍可解析，但成员与权限明确为空（fail closed）。
	if _, err := service.CreateUser(ctx, "user-2"); err != nil {
		t.Fatal(err)
	}
	mustBind(t, service, "app-a", "qq", "space", "bob", "user-2")
	resolved, err = service.ResolveIdentity(ctx, "app-a", "qq", "space", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Membership != nil || len(resolved.RoleIDs) != 0 || len(resolved.Permissions) != 0 {
		t.Fatalf("memberless context=%#v, want empty membership/roles/permissions", resolved)
	}

	// IdentityContextForUser 与 ResolveIdentity 返回一致的快照。
	direct, err := service.IdentityContextForUser(ctx, "app-a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if direct.UserID != "user-1" || !sortedEqual(direct.Permissions, []string{"bus.read", "bus.write"}) ||
		direct.BindingRevision != resolved.BindingRevision {
		t.Fatalf("IdentityContextForUser mismatch: %#v vs %#v", direct, resolved)
	}
}

func TestUserLifecycleAndAppMemberships(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	// 重复创建同一内部用户：ErrConflict（控制面显式创建，不幂等覆盖）。
	if _, err := service.CreateUser(ctx, "user-1"); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("duplicate user error=%v, want ErrConflict", err)
	}
	user, err := service.GetUser(ctx, "user-1")
	if err != nil || user.Status != identity.UserStatusActive {
		t.Fatalf("user=%#v err=%v", user, err)
	}
	// 初始无成员关系：ErrNotFound。
	if _, err := service.Membership(ctx, "app-a", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("initial membership error=%v, want ErrNotFound", err)
	}
	mustRole(t, service, "app-a", "member", "成员")
	mustMembership(t, service, "app-a", "user-1", "member")
	membership, err := service.Membership(ctx, "app-a", "user-1")
	if err != nil || membership.AppID != "app-a" || !sortedEqual(membership.RoleIDs, []string{"member"}) {
		t.Fatalf("membership=%#v err=%v", membership, err)
	}
	if members, err := service.MembersByApp(ctx, "app-a"); err != nil || len(members) != 1 || members[0].UserID != "user-1" {
		t.Fatalf("members=%#v err=%v", members, err)
	}
	// 成员关系整体替换角色集合。
	mustRole(t, service, "app-a", "admin", "管理员")
	mustMembership(t, service, "app-a", "user-1", "admin", "member")
	membership, err = service.Membership(ctx, "app-a", "user-1")
	if err != nil || !sortedEqual(membership.RoleIDs, []string{"admin", "member"}) {
		t.Fatalf("replaced membership=%#v err=%v", membership, err)
	}
	if err := service.RemoveMembership(ctx, "app-a", "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Membership(ctx, "app-a", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("after remove membership error=%v, want ErrNotFound", err)
	}
	// 明确的错误路径。
	if err := service.RemoveMembership(ctx, "app-a", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("remove missing membership error=%v, want ErrNotFound", err)
	}
	if _, err := service.DisableUser(ctx, "missing-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("disable missing user error=%v, want ErrNotFound", err)
	}
	if _, err := service.GetUser(ctx, "missing-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("get missing user error=%v, want ErrNotFound", err)
	}
	// 语法合法但不存在的 App：按不存在返回 ErrNotFound（fail closed），不是错误。
	if _, err := service.Membership(ctx, "missing-app", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("membership for unknown app error=%v, want ErrNotFound", err)
	}
	// 非法 App ID：ErrInvalid。
	if _, err := service.Membership(ctx, "Missing-App", "user-1"); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("invalid app error=%v, want ErrInvalid", err)
	}
}

func TestRoleLifecycleAndReferences(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustRole(t, service, "app-a", "member", "成员")
	role, err := service.GetRole(ctx, "app-a", "member")
	if err != nil || role.Name != "成员" || role.AppID != "app-a" {
		t.Fatalf("role=%#v err=%v", role, err)
	}
	if roles, err := service.ListRoles(ctx, "app-a"); err != nil || len(roles) != 1 {
		t.Fatalf("roles=%#v err=%v", roles, err)
	}
	// 引用不存在的角色：ErrRoleNotFound。
	if err := service.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"ghost"}}); !errors.Is(err, identity.ErrRoleNotFound) {
		t.Fatalf("membership with ghost role error=%v, want ErrRoleNotFound", err)
	}
	mustMembership(t, service, "app-a", "user-1", "member")
	// 仍被成员引用的角色禁止删除。
	if err := service.DeleteRole(ctx, "app-a", "member"); !errors.Is(err, identity.ErrRoleInUse) {
		t.Fatalf("delete in-use role error=%v, want ErrRoleInUse", err)
	}
	if err := service.RemoveMembership(ctx, "app-a", "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteRole(ctx, "app-a", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetRole(ctx, "app-a", "member"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("get deleted role error=%v, want ErrNotFound", err)
	}
	if err := service.DeleteRole(ctx, "app-a", "member"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("delete missing role error=%v, want ErrNotFound", err)
	}
	// 角色删除后其权限授予一并级联清除。
	mustRole(t, service, "app-b", "temp", "临时")
	mustGrantRole(t, service, "app-b", "temp", "bus.read")
	if err := service.DeleteRole(ctx, "app-b", "temp"); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionGrantValidationAndErrors(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustRole(t, service, "app-a", "member", "成员")
	mustMembership(t, service, "app-a", "user-1", "member")

	// 非法输入：同时/都不指定 subject。
	if err := service.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", RoleID: "member", Permission: "bus.read"}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("both subjects error=%v, want ErrInvalid", err)
	}
	if err := service.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", Permission: "bus.read"}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("no subject error=%v, want ErrInvalid", err)
	}
	// 非法权限字符串。
	if err := service.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "Bus.Read"}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("invalid permission error=%v, want ErrInvalid", err)
	}
	// 授予不存在的角色。
	if err := service.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", RoleID: "ghost", Permission: "bus.read"}); !errors.Is(err, identity.ErrRoleNotFound) {
		t.Fatalf("grant ghost role error=%v, want ErrRoleNotFound", err)
	}
	// 授予没有成员关系的用户。
	if _, err := service.CreateUser(ctx, "user-2"); err != nil {
		t.Fatal(err)
	}
	if err := service.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-2", Permission: "bus.read"}); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("grant memberless user error=%v, want ErrNotFound", err)
	}
	// 重复授予幂等成功。
	mustGrantUser(t, service, "app-a", "user-1", "bus.read")
	mustGrantUser(t, service, "app-a", "user-1", "bus.read")
	// 撤销未授予的权限：ErrNotFound。
	if err := service.RevokePermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("revoke missing permission error=%v, want ErrNotFound", err)
	}
}

// 绑定修订号随每次 App 级身份/授权变更递增；幂等写入不推进。
func TestBindingRevisionIncrementsOnGovernanceMutations(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	revision, err := service.BindingRevision(ctx, "app-a")
	if err != nil || revision != 0 {
		t.Fatalf("initial revision=%d err=%v, want 0", revision, err)
	}
	next := func() {
		t.Helper()
		revision, err = service.BindingRevision(ctx, "app-a")
		if err != nil {
			t.Fatal(err)
		}
	}
	wait := func(want int64) {
		t.Helper()
		next()
		if revision != want {
			t.Fatalf("revision=%d, want %d", revision, want)
		}
	}

	mustRole(t, service, "app-a", "member", "成员")
	wait(1)
	mustMembership(t, service, "app-a", "user-1", "member")
	wait(2)
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	wait(3)
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")
	wait(4)
	mustGrantRole(t, service, "app-a", "member", "bus.read")
	wait(5)
	if err := service.RevokePermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}
	wait(6)
	if err := service.UnbindExternalIdentity(ctx, "app-a", "qq", "space", "alice"); err != nil {
		t.Fatal(err)
	}
	wait(7)
	if _, err := service.DisableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	wait(8)
	if _, err := service.EnableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	wait(9)

	// 解绑后的重新绑定、撤销后的重新授予都会改变持久化状态，是真实变更：推进修订号。
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	wait(10)
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")
	wait(11)

	// 真正的幂等重复写入不推进修订号。
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	mustRole(t, service, "app-a", "member", "成员")
	mustMembership(t, service, "app-a", "user-1", "member")
	mustGrantUser(t, service, "app-a", "user-1", "bus.write")
	mustGrantRole(t, service, "app-a", "member", "bus.read")
	wait(11)
}

// 验收标准 2 的并发形态：并发绑定同一外部身份时，最终只归属一个用户。
func TestConcurrentBindIsMutuallyExclusive(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	for _, userID := range []string{"user-a", "user-b"} {
		if _, err := service.CreateUser(ctx, userID); err != nil {
			t.Fatal(err)
		}
	}
	const workers = 12
	results := make(chan error, workers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			target := "user-a"
			if index%2 == 1 {
				target = "user-b"
			}
			results <- service.BindExternalIdentity(ctx, identity.ExternalIdentity{
				AppID: "app-a", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "concurrent-user", UserID: target,
			})
		}(index)
	}
	close(start)
	group.Wait()
	close(results)
	ok := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, identity.ErrAlreadyBound):
			conflicts++
		default:
			t.Fatalf("unexpected bind error: %v", err)
		}
	}
	// 一半并发方指向 user-a、一半指向 user-b：只有被选中的那一半全部成功，
	// 另一半全部得到冲突，最终只存在一条绑定记录。
	if ok != workers/2 || conflicts != workers/2 {
		t.Fatalf("ok=%d conflicts=%d, want %d/%d", ok, conflicts, workers/2, workers/2)
	}
	bindings, err := store.ListExternalIdentities(ctx, "app-a", "user-a")
	if err != nil {
		t.Fatal(err)
	}
	otherBindings, err := store.ListExternalIdentities(ctx, "app-a", "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings)+len(otherBindings) != 1 {
		t.Fatalf("external identity bound to both users: user-a=%d user-b=%d", len(bindings), len(otherBindings))
	}
}
