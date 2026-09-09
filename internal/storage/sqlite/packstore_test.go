package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func testScope(appID string) packstore.Scope {
	return packstore.Scope{AppID: appID, PackageID: "test", Namespace: "test/pkg"}
}

func testSnapshotMeta(revision string) packstore.SnapshotMeta {
	importedAt := time.Now().UTC().Add(-time.Hour)
	return packstore.SnapshotMeta{
		Revision: revision, Source: "test-source", Authoritative: true,
		Complete: true, ImportedAt: importedAt, ValidUntil: importedAt.Add(2 * time.Hour),
	}
}

func TestPackageDocumentsCRUDIsAppScoped(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "documents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	docs := store.PackageDocuments()
	scope := testScope("app-a")

	if err := docs.Put(ctx, scope, "routes", "route-a", []byte(`{"id":"route-a","name":"A"}`)); err != nil {
		t.Fatal(err)
	}
	read, err := docs.Get(ctx, scope, "routes", "route-a")
	if err != nil || !read.Found || string(read.Document.Payload) != `{"id":"route-a","name":"A"}` {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	// 尚无 ReplaceSnapshot：put 写入的文档存在但没有生效快照。
	if read.MetaFound {
		t.Fatalf("put 后不应有生效快照：%#v", read)
	}
	// 跨 App 不可见：App 隔离在存储层强制。
	if missing, err := docs.Get(ctx, testScope("app-b"), "routes", "route-a"); err != nil || missing.Found {
		t.Fatalf("cross-app read=%#v err=%v, want not found", missing, err)
	}
	// Put 是 upsert。
	if err := docs.Put(ctx, scope, "routes", "route-a", []byte(`{"id":"route-a","name":"A2"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.Get(ctx, scope, "routes", "route-a"); err != nil {
		t.Fatal(err)
	}
	listed, err := docs.List(ctx, scope, "routes", 10, "")
	if err != nil || len(listed.Documents) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if err := docs.Delete(ctx, scope, "routes", "route-a"); err != nil {
		t.Fatal(err)
	}
	if err := docs.Delete(ctx, scope, "routes", "route-a"); !errors.Is(err, packstore.ErrNotFound) {
		t.Fatalf("second delete err=%v, want ErrNotFound", err)
	}
	// 越界资源请求 fail-closed。
	if err := docs.Put(ctx, scope, "routes", "route-a", []byte("not-json")); !errors.Is(err, packstore.ErrInvalidPayload) {
		t.Fatalf("invalid payload err=%v", err)
	}
	if _, err := docs.List(ctx, scope, "routes", packstore.MaxListLimit+1, ""); err == nil {
		t.Fatal("over-limit list unexpectedly succeeded")
	}
}

func TestPackageDocumentsUserScopeIsolation(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "user-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	docs := store.PackageDocuments()
	systemScope := testScope("app-a")
	userA := systemScope.UserScope("user-a")
	userB := systemScope.UserScope("user-b")

	// 播种系统快照与两个用户的个人文档。
	if err := docs.ReplaceSnapshot(ctx, systemScope, testSnapshotMeta("revision-1"), map[string][]packstore.Document{
		"routes": {{ID: "route-system", Payload: []byte(`{"id":"route-system"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := docs.Put(ctx, userA, "routes", "route-a", []byte(`{"id":"route-a"}`)); err != nil {
		t.Fatal(err)
	}
	if err := docs.Put(ctx, userB, "routes", "route-b", []byte(`{"id":"route-b"}`)); err != nil {
		t.Fatal(err)
	}
	// 用户看不到彼此的文档。
	if missing, err := docs.Get(ctx, userA, "routes", "route-b"); err != nil || missing.Found {
		t.Fatalf("user-a read user-b doc=%#v err=%v, want not found", missing, err)
	}
	// 系统作用域看不到任何个人文档。
	systemListed, err := docs.List(ctx, systemScope, "routes", 10, "")
	if err != nil || len(systemListed.Documents) != 1 || systemListed.Documents[0].ID != "route-system" {
		t.Fatalf("system listed=%#v err=%v", systemListed, err)
	}
	// 个人作用域看不到系统文档，也不携带快照元数据。
	userListed, err := docs.List(ctx, userA, "routes", 10, "")
	if err != nil || len(userListed.Documents) != 1 || userListed.Documents[0].ID != "route-a" {
		t.Fatalf("user listed=%#v err=%v", userListed, err)
	}
	if userListed.MetaFound {
		t.Fatalf("个人读取不应携带快照元数据：%#v", userListed)
	}
	// 个人 Put 是 upsert，且不污染系统作用域。
	if err := docs.Put(ctx, userA, "routes", "route-a", []byte(`{"id":"route-a","v":2}`)); err != nil {
		t.Fatal(err)
	}
	if updated, err := docs.Get(ctx, userA, "routes", "route-a"); err != nil || string(updated.Document.Payload) != `{"id":"route-a","v":2}` {
		t.Fatalf("user-a upsert=%#v err=%v", updated, err)
	}
	if systemRead, err := docs.Get(ctx, systemScope, "routes", "route-a"); err != nil || systemRead.Found {
		t.Fatalf("user write leaked into system scope: %#v err=%v", systemRead, err)
	}
	// 快照替换只清系统文档：个人文档原样保留。
	if err := docs.ReplaceSnapshot(ctx, systemScope, testSnapshotMeta("revision-2"), map[string][]packstore.Document{
		"routes": {{ID: "route-system-2", Payload: []byte(`{"id":"route-system-2"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if missing, err := docs.Get(ctx, systemScope, "routes", "route-system"); err != nil || missing.Found {
		t.Fatalf("old system doc=%#v err=%v, want replaced", missing, err)
	}
	if kept, err := docs.Get(ctx, userA, "routes", "route-a"); err != nil || !kept.Found {
		t.Fatalf("user doc after snapshot replace=%#v err=%v, want preserved", kept, err)
	}
	// 个人作用域的快照替换被拒绝。
	if err := docs.ReplaceSnapshot(ctx, userA, testSnapshotMeta("revision-user"), map[string][]packstore.Document{
		"routes": {{ID: "x", Payload: []byte(`{}`)}},
	}); !errors.Is(err, packstore.ErrInvalidScope) {
		t.Fatalf("user snapshot replace err=%v, want ErrInvalidScope", err)
	}
	// 个人删除只影响本人文档。
	if err := docs.Delete(ctx, userA, "routes", "route-b"); !errors.Is(err, packstore.ErrNotFound) {
		t.Fatalf("user-a delete user-b doc err=%v, want ErrNotFound", err)
	}
}

func TestReplaceSnapshotIsAtomicAndPreservesLastCompleteVersion(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	docs := store.PackageDocuments()
	scope := testScope("app")

	original := map[string][]packstore.Document{
		"routes": {{ID: "route-a", Payload: []byte(`{"id":"route-a","name":"A"}`)}},
	}
	if err := docs.ReplaceSnapshot(ctx, scope, testSnapshotMeta("revision-1"), original); err != nil {
		t.Fatal(err)
	}
	// 重复文档 ID 的候选快照必须整体被拒。
	broken := map[string][]packstore.Document{
		"routes": {
			{ID: "duplicate", Payload: []byte(`{}`)},
			{ID: "duplicate", Payload: []byte(`{}`)},
		},
	}
	if err := docs.ReplaceSnapshot(ctx, scope, testSnapshotMeta("revision-2"), broken); !errors.Is(err, packstore.ErrInvalidSnapshot) {
		t.Fatalf("broken snapshot err=%v, want ErrInvalidSnapshot", err)
	}
	// 上一完整版本原样保留。
	listed, err := docs.List(ctx, scope, "routes", 10, "")
	if err != nil || !listed.MetaFound || listed.Meta.Revision != "revision-1" {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if len(listed.Documents) != 1 || listed.Documents[0].ID != "route-a" {
		t.Fatalf("documents=%#v", listed.Documents)
	}
	// 缺失有效期的快照元数据被拒。
	invalidMeta := testSnapshotMeta("revision-3")
	invalidMeta.ValidUntil = time.Time{}
	if err := docs.ReplaceSnapshot(ctx, scope, invalidMeta, original); !errors.Is(err, packstore.ErrInvalidSnapshot) {
		t.Fatalf("invalid meta err=%v, want ErrInvalidSnapshot", err)
	}
}

func TestPackageSnapshotQueriesRequireAnActiveRevision(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "missing-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	docs := store.PackageDocuments()
	listed, err := docs.List(context.Background(), testScope("app"), "routes", 10, "")
	if err != nil || listed.MetaFound || len(listed.Documents) != 0 {
		t.Fatalf("listed=%#v err=%v, want no snapshot and no documents", listed, err)
	}
}

func TestConcurrentPackageSnapshotReadersNeverObserveMixedRevision(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "concurrent-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	docs := store.PackageDocuments()
	scope := testScope("app")
	makeSnapshot := func(revision string) map[string][]packstore.Document {
		return map[string][]packstore.Document{
			"routes": {{ID: "route-a", Payload: []byte(`{"id":"route-a","source_revision":"` + revision + `"}`)}},
		}
	}
	if err := docs.ReplaceSnapshot(ctx, scope, testSnapshotMeta("revision-a"), makeSnapshot("revision-a")); err != nil {
		t.Fatal(err)
	}

	failures := make(chan error, 8)
	var readers sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for attempt := 0; attempt < 30; attempt++ {
				listed, err := docs.List(ctx, scope, "routes", 10, "")
				if err != nil {
					failures <- err
					return
				}
				// 文档与元数据由实现保证同源：任何读取都不得观察到跨修订混合。
				if !listed.MetaFound || len(listed.Documents) != 1 {
					failures <- fmt.Errorf("mixed snapshot: unexpected read %#v", listed)
					return
				}
				var decoded struct {
					SourceRevision string `json:"source_revision"`
				}
				if err := json.Unmarshal(listed.Documents[0].Payload, &decoded); err != nil {
					failures <- err
					return
				}
				if decoded.SourceRevision != listed.Meta.Revision {
					failures <- errors.New("mixed snapshot: document revision != snapshot revision")
					return
				}
			}
		}()
	}
	for attempt := 0; attempt < 20; attempt++ {
		revision := "revision-a"
		if attempt%2 == 1 {
			revision = "revision-b"
		}
		if err := docs.ReplaceSnapshot(ctx, scope, testSnapshotMeta(revision), makeSnapshot(revision)); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}
