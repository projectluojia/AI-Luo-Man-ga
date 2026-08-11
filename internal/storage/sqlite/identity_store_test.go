package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func openIdentityStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "identity-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustCreateUser(t *testing.T, store *sqlite.Store, userID string) {
	t.Helper()
	if _, err := store.CreateUser(context.Background(), identity.User{UserID: userID, Status: identity.UserStatusActive}); err != nil {
		t.Fatalf("create user %s: %v", userID, err)
	}
}

// 迁移 15 建立身份 Schema，并通过数据库约束拒绝非法状态。
func TestIdentityMigration15CreatesIdentitySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity-migration.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	mustCreateUser(t, store, "user-2")
	if err := store.EnsureRole(ctx, identity.Role{AppID: "app-a", RoleID: "member", Name: "成员"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-a", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "p1", UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", RoleID: "member", Permission: "bus.read"}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}
	permissions, err := store.EffectivePermissions(ctx, "app-a", "user-1")
	if err != nil || len(permissions) != 2 {
		t.Fatalf("effective permissions=%v err=%v", permissions, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 15 {
		t.Fatalf("schema version=%d err=%v, want 15", version, err)
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('users','external_identities','app_memberships','roles','permission_grants','identity_binding_revisions')`).Scan(&tables); err != nil || tables != 6 {
		t.Fatalf("identity tables=%d err=%v, want 6", tables, err)
	}
	// CHECK：非法用户状态被数据库拒绝。
	if _, err := db.Exec(`INSERT INTO users(user_id,status,created_at) VALUES('bad','banned','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted invalid user status")
	}
	// CHECK：active 状态不允许带 disabled_at。
	if _, err := db.Exec(`INSERT INTO users(user_id,status,created_at,disabled_at) VALUES('bad','active','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`); err == nil {
		t.Fatal("database accepted active user with disabled_at")
	}
	// CHECK：disabled 状态必须带 disabled_at。
	if _, err := db.Exec(`INSERT INTO users(user_id,status,created_at) VALUES('bad','disabled','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted disabled user without disabled_at")
	}
	// 唯一键：重复 user_id 被拒绝。
	if _, err := db.Exec(`INSERT INTO users(user_id,status,created_at) VALUES('user-1','active','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted duplicate user_id")
	}
	// CHECK：permission_grants 必须恰好一个 subject。
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,permission,granted_at) VALUES('app-a','p','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted grant without subject")
	}
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,user_id,role_id,permission,granted_at) VALUES('app-a','user-1','member','p','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted grant with both subjects")
	}
	// CHECK：role_ids 必须是合法 JSON 数组。
	if _, err := db.Exec(`INSERT INTO app_memberships(app_id,user_id,role_ids,created_at,updated_at) VALUES('app-a','user-2','not-json','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted non-JSON role_ids")
	}
}

// 数据库唯一约束与外键是并发绑定的最终裁决者。
func TestIdentityDatabaseEnforcesUniqueKeysAndForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity-constraints.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateUser(t, store, "user-a")
	mustCreateUser(t, store, "user-b")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	// 唯一键：同一外部身份不能绑定两个用户。
	if _, err := db.Exec(`INSERT INTO external_identities(app_id,platform,platform_space_id,platform_user_id,user_id,bound_at) VALUES('app','qq','space','p1','user-a','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO external_identities(app_id,platform,platform_space_id,platform_user_id,user_id,bound_at) VALUES('app','qq','space','p1','user-b','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted the same external identity bound to two users")
	}
	// 外键：外部身份必须指向已存在的内部用户。
	if _, err := db.Exec(`INSERT INTO external_identities(app_id,platform,platform_space_id,platform_user_id,user_id,bound_at) VALUES('app','qq','space','p2','ghost','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted a binding to a missing user")
	}
	// 唯一键：同一 (app_id, user_id) 只能有一个成员关系。
	if _, err := db.Exec(`INSERT INTO app_memberships(app_id,user_id,role_ids,created_at,updated_at) VALUES('app','user-a','[]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first membership: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO app_memberships(app_id,user_id,role_ids,created_at,updated_at) VALUES('app','user-a','[]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted duplicate membership")
	}
	// 外键：成员关系必须指向已存在的内部用户。
	if _, err := db.Exec(`INSERT INTO app_memberships(app_id,user_id,role_ids,created_at,updated_at) VALUES('app','ghost','[]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted a membership for a missing user")
	}
	// 外键：角色权限授予必须指向已存在的角色。
	if _, err := db.Exec(`INSERT INTO roles(app_id,role_id,name,description,created_at) VALUES('app','member','成员','','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first role: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,role_id,permission,granted_at) VALUES('app','ghost-role','bus.read','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted a grant for a missing role")
	}
	// 外键：用户直接授予必须指向已存在的成员关系。
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,user_id,permission,granted_at) VALUES('app','user-b','bus.read','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted a grant for a memberless user")
	}
	// 部分唯一索引：同一用户同一权限只允许一条授予记录。
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,user_id,permission,granted_at) VALUES('app','user-a','bus.write','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first user grant: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,user_id,permission,granted_at) VALUES('app','user-a','bus.write','2026-01-01T00:00:01Z')`); err == nil {
		t.Fatal("database accepted duplicate user grant")
	}
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,role_id,permission,granted_at) VALUES('app','member','bus.read','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("first role grant: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO permission_grants(app_id,role_id,permission,granted_at) VALUES('app','member','bus.read','2026-01-01T00:00:01Z')`); err == nil {
		t.Fatal("database accepted duplicate role grant")
	}
}

// 验收标准 3 的存储层形态：全部查询同时约束 app_id，跨 App 按不存在处理。
func TestIdentityStoreEnforcesAppIsolation(t *testing.T) {
	store := openIdentityStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	mustCreateUser(t, store, "user-2")

	if err := store.EnsureRole(ctx, identity.Role{AppID: "app-a", RoleID: "member", Name: "成员"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-a", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "p1", UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", RoleID: "member", Permission: "bus.read"}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}

	permissions, err := store.EffectivePermissions(ctx, "app-a", "user-1")
	if err != nil || len(permissions) != 2 {
		t.Fatalf("app-a permissions=%v err=%v", permissions, err)
	}
	// 同一用户在 App B 没有任何权限与成员关系。
	permissionsB, err := store.EffectivePermissions(ctx, "app-b", "user-1")
	if err != nil || len(permissionsB) != 0 {
		t.Fatalf("app-b permissions=%v err=%v, want empty", permissionsB, err)
	}
	if _, err := store.GetMembership(ctx, "app-b", "user-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("cross-app membership error=%v, want ErrNotFound", err)
	}
	if memberships, err := store.ListMemberships(ctx, "app-b"); err != nil || len(memberships) != 0 {
		t.Fatalf("app-b memberships=%#v err=%v", memberships, err)
	}
	if _, err := store.GetRole(ctx, "app-b", "member"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("cross-app role error=%v, want ErrNotFound", err)
	}
	if _, err := store.GetExternalIdentity(ctx, "app-b", "qq", "space", "p1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("cross-app binding error=%v, want ErrNotFound", err)
	}
	// App 级绑定隔离：同一外部身份可以在另一 App 绑定到另一个用户。
	if err := store.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-b", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "p1", UserID: "user-2",
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetExternalIdentity(ctx, "app-a", "qq", "space", "p1")
	if err != nil || binding.UserID != "user-1" {
		t.Fatalf("app-a binding=%#v err=%v", binding, err)
	}
	// 撤销 App A 的授予不影响 App B。
	if err := store.RevokePermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}
	permissionsA, err := store.EffectivePermissions(ctx, "app-a", "user-1")
	if err != nil || len(permissionsA) != 1 {
		t.Fatalf("app-a permissions after revoke=%v err=%v", permissionsA, err)
	}
}

// 验收标准 2 的并发形态（race）：唯一约束保证同一外部身份最终只归属一个用户。
func TestIdentityConcurrentBindIsMutuallyExclusive(t *testing.T) {
	store := openIdentityStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-a")
	mustCreateUser(t, store, "user-b")

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
			results <- store.BindExternalIdentity(ctx, identity.ExternalIdentity{
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
	if ok != workers/2 || conflicts != workers/2 {
		t.Fatalf("ok=%d conflicts=%d, want %d/%d", ok, conflicts, workers/2, workers/2)
	}
	for _, userID := range []string{"user-a", "user-b"} {
		bindings, err := store.ListExternalIdentities(ctx, "app-a", userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindings) > 1 {
			t.Fatalf("external identity bound to %d users", len(bindings))
		}
	}
}

// 成员直接授予随成员删除级联清除；角色权限随角色删除级联清除。
func TestPermissionGrantsCascadeOnMembershipAndRoleDeletion(t *testing.T) {
	store := openIdentityStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	if err := store.EnsureRole(ctx, identity.Role{AppID: "app-a", RoleID: "member", Name: "成员"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", UserID: "user-1", Permission: "bus.write"}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantPermission(ctx, identity.PermissionGrant{AppID: "app-a", RoleID: "member", Permission: "bus.read"}); err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		permissions, err := store.EffectivePermissions(ctx, "app-a", "user-1")
		if err != nil || len(permissions) != want {
			t.Fatalf("permissions=%v err=%v, want %d", permissions, err, want)
		}
	}
	check(2)
	// 删除成员：直接授予级联清除，但角色与角色权限仍然存在。
	if err := store.RemoveMembership(ctx, "app-a", "user-1"); err != nil {
		t.Fatal(err)
	}
	check(0)
	if _, err := store.GetRole(ctx, "app-a", "member"); err != nil {
		t.Fatalf("role should survive member removal: %v", err)
	}
	// 重新加入成员并引用角色：角色权限恢复。
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	check(1)
	// 删除角色：先移除引用该角色的成员，角色权限授予再级联清除。
	if err := store.RemoveMembership(ctx, "app-a", "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRole(ctx, "app-a", "member"); err != nil {
		t.Fatal(err)
	}
	check(0)
}

// 用户禁用/启用状态机在存储层保持一致性。
func TestIdentityStoreUserStatusLifecycle(t *testing.T) {
	store := openIdentityStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	if err := store.EnsureRole(ctx, identity.Role{AppID: "app-a", RoleID: "member", Name: "成员"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-a", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	disabled, err := store.SetUserStatus(ctx, "user-1", identity.UserStatusDisabled, now)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != identity.UserStatusDisabled || disabled.DisabledAt == nil {
		t.Fatalf("disabled user=%#v", disabled)
	}
	// 禁用用户不能新增绑定。
	if err := store.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-a", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "p1", UserID: "user-1",
	}); !errors.Is(err, identity.ErrUserDisabled) {
		t.Fatalf("bind disabled user error=%v, want ErrUserDisabled", err)
	}
	// 幂等禁用：保留原禁用时间，不推进修订号。
	revisionBefore, err := store.BindingRevision(ctx, "app-a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.SetUserStatus(ctx, "user-1", identity.UserStatusDisabled, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if again.DisabledAt == nil || !again.DisabledAt.Equal(*disabled.DisabledAt) {
		t.Fatalf("idempotent disable changed DisabledAt: %#v vs %#v", again, disabled)
	}
	revisionAfter, err := store.BindingRevision(ctx, "app-a")
	if err != nil || revisionAfter != revisionBefore {
		t.Fatalf("idempotent disable moved revision %d -> %d err=%v", revisionBefore, revisionAfter, err)
	}
	enabled, err := store.SetUserStatus(ctx, "user-1", identity.UserStatusActive, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Status != identity.UserStatusActive || enabled.DisabledAt != nil {
		t.Fatalf("enabled user=%#v", enabled)
	}
	// 不存在的用户：明确状态。
	if _, err := store.SetUserStatus(ctx, "ghost", identity.UserStatusDisabled, now); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("set status for missing user error=%v, want ErrNotFound", err)
	}
	// 非法状态值被拒绝。
	if _, err := store.SetUserStatus(ctx, "user-1", "banned", now); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("invalid status error=%v, want ErrInvalid", err)
	}
}

// 用户状态变更影响其全部可解析 App 的修订号：仅有绑定没有成员的 App 同样推进。
func TestIdentityUserStatusBumpsRevisionInAllResolvableApps(t *testing.T) {
	store := openIdentityStore(t)
	ctx := context.Background()
	mustCreateUser(t, store, "user-1")
	// 绑定-only 的 App 与成员 App 都能通过该用户解析身份。
	if err := store.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: "app-bind", Platform: "qq", PlatformSpaceID: "space", PlatformUserID: "p1", UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureRole(ctx, identity.Role{AppID: "app-member", RoleID: "member", Name: "成员"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMembership(ctx, identity.AppMembership{AppID: "app-member", UserID: "user-1", RoleIDs: []string{"member"}}); err != nil {
		t.Fatal(err)
	}
	if revision, err := store.BindingRevision(ctx, "app-bind"); err != nil || revision != 1 {
		t.Fatalf("app-bind revision=%d err=%v, want 1", revision, err)
	}
	if revision, err := store.BindingRevision(ctx, "app-member"); err != nil || revision != 2 {
		t.Fatalf("app-member revision=%d err=%v, want 2", revision, err)
	}
	if _, err := store.SetUserStatus(ctx, "user-1", identity.UserStatusDisabled, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 禁用后两个 App 的修订号都推进；启用同样覆盖绑定-only App。
	for appID, want := range map[string]int64{"app-bind": 2, "app-member": 3} {
		revision, err := store.BindingRevision(ctx, appID)
		if err != nil || revision != want {
			t.Fatalf("%s revision after disable=%d err=%v, want %d", appID, revision, err, want)
		}
	}
	if _, err := store.SetUserStatus(ctx, "user-1", identity.UserStatusActive, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for appID, want := range map[string]int64{"app-bind": 3, "app-member": 4} {
		revision, err := store.BindingRevision(ctx, appID)
		if err != nil || revision != want {
			t.Fatalf("%s revision after enable=%d err=%v, want %d", appID, revision, err, want)
		}
	}
}
