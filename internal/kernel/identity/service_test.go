package identity_test

import (
	"context"
	"errors"
	"path/filepath"
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

// 验收标准 1：外部平台 ID 不能直接充当内部 user_id。
func TestExternalPlatformIDNeverBecomesInternalUserID(t *testing.T) {
	service, store := newTestService(t)
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
	// 平台标识从未被当作内部用户：以平台 ID 查询用户必须明确不存在，
	// 权限查询按不存在返回空集合。
	if _, err := store.GetUser(ctx, "external-alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("platform ID became a user: %v", err)
	}
	if permissions, err := store.EffectivePermissions(ctx, "app-a", "external-alice"); err != nil || len(permissions) != 0 {
		t.Fatalf("platform ID resolved as internal user: %v err=%v", permissions, err)
	}
	if _, err := store.GetMembership(ctx, "app-a", "external-alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("platform ID resolved as internal member: %v", err)
	}
}

// 验收标准 5：身份不存在时返回明确状态，不自动创建匿名权威用户。
func TestMissingIdentityReturnsExplicitStatusWithoutAutoCreate(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	// 未绑定身份解析：ErrNotFound，且不会产生任何用户。
	if _, err := service.ResolveIdentity(ctx, "app", "qq", "space", "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("resolve unbound identity error=%v, want ErrNotFound", err)
	}
	if _, err := store.GetUser(ctx, "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("anonymous user was auto-created: %v", err)
	}
	// 绑定到不存在的内部用户：ErrNotFound。
	err := service.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app", Platform: "wechat", PlatformSpaceID: "space", PlatformUserID: "wx-1", UserID: "ghost-user",
	})
	if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("bind to missing user error=%v, want ErrNotFound", err)
	}
	if _, err := store.GetUser(ctx, "ghost-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("binding auto-created an anonymous user: %v", err)
	}
	// 解绑不存在的身份：ErrNotFound。
	if err := service.UnbindExternalIdentity(ctx, "app", "qq", "space", "unknown-platform-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unbind missing identity error=%v, want ErrNotFound", err)
	}
}

// 验收标准 2：同一外部身份不能被绑定给两个用户；同用户重复绑定幂等成功。
func TestSameExternalIdentityCannotBindTwoUsers(t *testing.T) {
	service, store := newTestService(t)
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
	revisionBefore, err := store.BindingRevision(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	mustBind(t, service, "app", "qq", "space", "platform-user", "user-a")
	revisionAfter, err := store.BindingRevision(ctx, "app")
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

// 验收标准 3：绑定与成员关系按 App 隔离；跨 App 解析/读取一律不存在。
func TestIdentityNeverLeaksAcrossApps(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustMembership(t, service, "app-a", "user-1")
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")

	// 同一用户在 App B 没有任何成员关系与权限。
	if _, err := store.GetMembership(ctx, "app-b", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("app-b membership should be absent: %v", err)
	}
	if permissions, err := store.EffectivePermissions(ctx, "app-b", "user-1"); err != nil || len(permissions) != 0 {
		t.Fatalf("app-b permissions=%v err=%v, want empty", permissions, err)
	}
	// App 级绑定隔离：身份在 app-a 绑定，在 app-b 解析必须明确不存在。
	if _, err := service.ResolveIdentity(ctx, "app-b", "qq", "space", "alice"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("cross-app resolve error=%v, want ErrNotFound", err)
	}
	// 用户在 App B 重新成为成员后，App A 的成员关系不受影响。
	mustMembership(t, service, "app-b", "user-1")
	if membership, err := store.GetMembership(ctx, "app-b", "user-1"); err != nil || membership.UserID != "user-1" {
		t.Fatalf("app-b membership=%#v err=%v", membership, err)
	}
	if _, err := store.GetMembership(ctx, "app-a", "user-1"); err != nil {
		t.Fatalf("app-a membership lost: %v", err)
	}
}

// 验收标准 4：禁用在下一次查询时刻立即生效（身份快照实时计算，不缓存）。
func TestDisableTakesEffectImmediately(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustMembership(t, service, "app-a", "user-1")
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")

	if _, err := service.DisableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
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
	// 禁用不存在的用户：ErrNotFound。
	if _, err := service.DisableUser(ctx, "missing-user"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("disable missing user error=%v, want ErrNotFound", err)
	}
}

// IdentityContext 至少包含 UserID、AppID、成员关系与绑定修订号；
// 无成员关系的用户快照为空（fail closed）。
func TestIdentityContextCarriesMembershipAndRevision(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	mustMembership(t, service, "app-a", "user-1")
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
	if len(resolved.RoleIDs) != 0 || len(resolved.Permissions) != 0 {
		t.Fatalf("context role_ids=%v permissions=%v, want empty", resolved.RoleIDs, resolved.Permissions)
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
}

func TestUserLifecycleAndAppMemberships(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	// 重复创建同一内部用户：ErrConflict（控制面显式创建，不幂等覆盖）。
	if _, err := service.CreateUser(ctx, "user-1"); !errors.Is(err, identity.ErrConflict) {
		t.Fatalf("duplicate user error=%v, want ErrConflict", err)
	}
	user, err := store.GetUser(ctx, "user-1")
	if err != nil || user.Status != identity.UserStatusActive {
		t.Fatalf("user=%#v err=%v", user, err)
	}
	// 初始无成员关系：ErrNotFound。
	if _, err := store.GetMembership(ctx, "app-a", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("initial membership error=%v, want ErrNotFound", err)
	}
	mustMembership(t, service, "app-a", "user-1")
	membership, err := store.GetMembership(ctx, "app-a", "user-1")
	if err != nil || membership.AppID != "app-a" || len(membership.RoleIDs) != 0 {
		t.Fatalf("membership=%#v err=%v", membership, err)
	}
	// 成员关系整体替换：写入空角色集合即清除角色。
	mustMembership(t, service, "app-a", "user-1")
	// 语法合法但不存在的 App：按不存在返回 ErrNotFound（fail closed），不是错误。
	if _, err := store.GetMembership(ctx, "missing-app", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("membership for unknown app error=%v, want ErrNotFound", err)
	}
	// 非法 App ID：ErrInvalid。
	if _, err := store.GetMembership(ctx, "Missing-App", "user-1"); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("invalid app error=%v, want ErrInvalid", err)
	}
}

// 成员关系引用不存在的角色：ErrRoleNotFound（SetMembership 内联校验，不自动创建）。
func TestMembershipRejectsUnknownRole(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"ghost"}}); !errors.Is(err, identity.ErrRoleNotFound) {
		t.Fatalf("membership with ghost role error=%v, want ErrRoleNotFound", err)
	}
}

// 绑定修订号随每次 App 级身份/授权变更递增；幂等写入不推进。
func TestBindingRevisionIncrementsOnGovernanceMutations(t *testing.T) {
	service, store := newTestService(t)
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	revision, err := store.BindingRevision(ctx, "app-a")
	if err != nil || revision != 0 {
		t.Fatalf("initial revision=%d err=%v, want 0", revision, err)
	}
	next := func() {
		t.Helper()
		revision, err = store.BindingRevision(ctx, "app-a")
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

	mustMembership(t, service, "app-a", "user-1")
	wait(1)
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	wait(2)

	// 幂等重复写入不推进修订号。
	mustMembership(t, service, "app-a", "user-1")
	mustBind(t, service, "app-a", "qq", "space", "alice", "user-1")
	wait(2)

	if err := service.UnbindExternalIdentity(ctx, "app-a", "qq", "space", "alice"); err != nil {
		t.Fatal(err)
	}
	wait(3)
	if _, err := service.DisableUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	wait(4)
}

// 验收标准 2 的并发形态：并发绑定同一外部身份时，最终只归属一个用户。
func TestConcurrentBindIsMutuallyExclusive(t *testing.T) {
	service, _ := newTestService(t)
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
}
