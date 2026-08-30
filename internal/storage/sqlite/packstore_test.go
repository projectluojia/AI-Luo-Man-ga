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
	return packstore.Scope{AppID: appID, Namespace: "test/pkg"}
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
