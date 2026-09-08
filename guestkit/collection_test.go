package guestkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeLister 是分页读取测试的假存储：按 doc_id 升序分页，并可注入读取失败。
type fakeLister struct {
	docs       map[string]map[string]json.RawMessage
	meta       SnapshotMeta
	failOnPage int // 第几次 List 调用（1 起）返回错误；0 表示不失败
	calls      int
}

func (f *fakeLister) List(_ Scope, collection string, limit int, afterID string) (ListPage, error) {
	f.calls++
	if f.failOnPage == f.calls {
		return ListPage{}, errors.New("store list call failed")
	}
	collectionDocs := f.docs[collection]
	ids := make([]string, 0, len(collectionDocs))
	for id := range collectionDocs {
		if afterID != "" && id <= afterID {
			continue
		}
		ids = append(ids, id)
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	docs := make([]Document, 0, len(ids))
	for _, id := range ids {
		docs = append(docs, Document{ID: id, Payload: collectionDocs[id]})
	}
	return ListPage{Docs: docs, Meta: f.meta, MetaFound: f.meta.SourceRevision != ""}, nil
}

func seedLister(revision string) *fakeLister {
	return &fakeLister{
		docs: map[string]map[string]json.RawMessage{
			"routes": {
				"route-a": json.RawMessage(`{"id":"route-a","source_revision":"` + revision + `"}`),
				"route-b": json.RawMessage(`{"id":"route-b","source_revision":"` + revision + `"}`),
				"route-c": json.RawMessage(`{"id":"route-c","source_revision":"` + revision + `"}`),
			},
		},
		meta: SnapshotMeta{
			SourceRevision: revision, Source: "test", Authoritative: true,
			Complete: true, ImportedAt: clock().Add(-time.Hour), ValidUntil: clock().Add(time.Hour),
		},
	}
}

func TestFetchSnapshotCollectionPaginatesAndDecodes(t *testing.T) {
	source := seedLister("rev-1")
	snapshot, err := FetchSnapshotCollection[map[string]any](source, "routes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Documents) != 3 || !snapshot.MetaFound || snapshot.Meta.SourceRevision != "rev-1" {
		t.Fatalf("snapshot=%+v", snapshot)
	}

	// 文档修订与快照修订不一致按数据不可用拒绝。
	source.docs["routes"]["route-d"] = json.RawMessage(`{"id":"route-d","source_revision":"rev-old"}`)
	if _, err := FetchSnapshotCollection[map[string]any](source, "routes", nil); !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("stale doc err=%v", err)
	}
}

// driftLister 在第二页返回不同快照修订：分页中途修订漂移必须整体失败。
type driftLister struct{}

func (d driftLister) List(_ Scope, collection string, limit int, afterID string) (ListPage, error) {
	revision := "rev-1"
	if afterID != "" {
		// 第二页：同一文档载荷、不同快照修订。
		revision = "rev-2"
	}
	// 整页返回才能触发第二页请求。
	docs := make([]Document, 0, limit)
	for index := 0; index < limit; index++ {
		id := "route-" + string(rune('a'+index%26)) + fmt.Sprint(index)
		docs = append(docs, Document{
			ID:      id,
			Payload: json.RawMessage(`{"id":"` + id + `","source_revision":"rev-1"}`),
		})
	}
	return ListPage{Docs: docs, Meta: SnapshotMeta{SourceRevision: revision}, MetaFound: true}, nil
}

func TestFetchSnapshotCollectionRejectsRevisionDriftMidPage(t *testing.T) {
	_, err := FetchSnapshotCollection[map[string]any](driftLister{}, "routes", nil)
	if !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("drift err=%v", err)
	}
}

func TestGovernedSnapshotCollectionRequiresMeta(t *testing.T) {
	source := seedLister("")
	if _, err := GovernedSnapshotCollection[map[string]any](source, "routes", nil); !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("missing meta err=%v", err)
	}
	source = seedLister("rev-1")
	if _, err := GovernedSnapshotCollection[map[string]any](source, "routes", nil); err != nil {
		t.Fatal(err)
	}
}

func TestUserCollectionReadsAllPagesAndDecodes(t *testing.T) {
	type course struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	source := &fakeLister{docs: map[string]map[string]json.RawMessage{}}
	payload, _ := json.Marshal(course{ID: "course-1", Title: "高数"})
	source.docs["courses-t1"] = map[string]json.RawMessage{"course-1": payload}
	payload, _ = json.Marshal(course{ID: "course-2", Title: "物理"})
	source.docs["courses-t1"]["course-2"] = payload

	documents, err := UserCollection[course](source, "courses-t1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0].ID != "course-1" || documents[1].Title != "物理" {
		t.Fatalf("documents=%+v", documents)
	}

	// 自定义解码器生效。
	documents, err = UserCollection(source, "courses-t1", func(raw json.RawMessage) (course, error) {
		var item course
		if err := json.Unmarshal(raw, &item); err != nil {
			return item, err
		}
		item.Title += "!"
		return item, nil
	})
	if err != nil || documents[0].Title != "高数!" {
		t.Fatalf("custom decode documents=%+v err=%v", documents, err)
	}
}

func TestUserCollectionFailsWholeOnStorageError(t *testing.T) {
	source := seedLister("rev-1")
	source.failOnPage = 1
	if _, err := UserCollection[map[string]any](source, "routes", nil); err == nil {
		t.Fatal("storage error must fail whole read")
	}
}
