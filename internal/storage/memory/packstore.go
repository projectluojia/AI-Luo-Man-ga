// Package memory 提供面向测试的内存存储适配器，实现与生产适配器相同的窄端口。
package memory

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
)

// Documents 是 packstore.Store 的内存实现：仅用于测试装配。
// 校验路径与生产实现一致（复用 packstore.Validate*），隔离按 Scope 精确匹配；
// 读取与生产实现一样返回当前快照元数据视图（内存锁下天然一致）。
type Documents struct {
	mu        sync.Mutex
	documents map[string]map[string]map[string]json.RawMessage // scope → collection → id → payload
	snapshots map[string]packstore.SnapshotMeta                // scope → 当前快照
}

// NewDocuments 构造空内存文档存储。
func NewDocuments() *Documents {
	return &Documents{
		documents: map[string]map[string]map[string]json.RawMessage{},
		snapshots: map[string]packstore.SnapshotMeta{},
	}
}

func scopeKey(scope packstore.Scope) string {
	return scope.AppID + "\x00" + scope.Namespace
}

func (m *Documents) collection(scope packstore.Scope, name string) map[string]json.RawMessage {
	byCollection, ok := m.documents[scopeKey(scope)]
	if !ok {
		byCollection = map[string]map[string]json.RawMessage{}
		m.documents[scopeKey(scope)] = byCollection
	}
	collection, ok := byCollection[name]
	if !ok {
		collection = map[string]json.RawMessage{}
		byCollection[name] = collection
	}
	return collection
}

func (m *Documents) readMeta(scope packstore.Scope) (packstore.SnapshotMeta, bool) {
	meta, ok := m.snapshots[scopeKey(scope)]
	return meta, ok
}

func (m *Documents) Get(_ context.Context, scope packstore.Scope, collection, id string) (packstore.DocumentRead, error) {
	if err := validateAccess(scope, collection, id, 0, ""); err != nil {
		return packstore.DocumentRead{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta, metaFound := m.readMeta(scope)
	payload, ok := m.collection(scope, collection)[id]
	if !ok {
		return packstore.DocumentRead{Meta: meta, MetaFound: metaFound, Found: false}, nil
	}
	return packstore.DocumentRead{
		Meta: meta, MetaFound: metaFound, Found: true,
		Document: packstore.Document{ID: id, Payload: payload},
	}, nil
}

func (m *Documents) Put(_ context.Context, scope packstore.Scope, collection, id string, payload json.RawMessage) error {
	if err := validateAccess(scope, collection, id, 0, ""); err != nil {
		return err
	}
	if err := packstore.ValidatePayload(payload); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collection(scope, collection)[id] = payload
	return nil
}

func (m *Documents) Delete(_ context.Context, scope packstore.Scope, collection, id string) error {
	if err := validateAccess(scope, collection, id, 0, ""); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.collection(scope, collection)[id]; !ok {
		return packstore.ErrNotFound
	}
	delete(m.collection(scope, collection), id)
	return nil
}

func (m *Documents) List(_ context.Context, scope packstore.Scope, collection string, limit int, afterID string) (packstore.CollectionRead, error) {
	if err := validateAccess(scope, collection, "", limit, afterID); err != nil {
		return packstore.CollectionRead{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta, metaFound := m.readMeta(scope)
	collectionDocs := m.collection(scope, collection)
	ids := make([]string, 0, len(collectionDocs))
	for id := range collectionDocs {
		if afterID == "" || id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	documents := make([]packstore.Document, 0, len(ids))
	for _, id := range ids {
		documents = append(documents, packstore.Document{ID: id, Payload: collectionDocs[id]})
	}
	return packstore.CollectionRead{Meta: meta, MetaFound: metaFound, Documents: documents}, nil
}

func (m *Documents) ReplaceSnapshot(_ context.Context, scope packstore.Scope, meta packstore.SnapshotMeta, collections map[string][]packstore.Document) error {
	if err := packstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := packstore.ValidateSnapshot(meta, collections); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[scopeKey(scope)] = map[string]map[string]json.RawMessage{}
	for collection, documents := range collections {
		byID := map[string]json.RawMessage{}
		for _, document := range documents {
			byID[document.ID] = document.Payload
		}
		m.documents[scopeKey(scope)][collection] = byID
	}
	m.snapshots[scopeKey(scope)] = meta
	return nil
}

// validateAccess 复用端口校验：List 场景 id 为空、limit 生效；其余场景相反。
func validateAccess(scope packstore.Scope, collection, id string, limit int, afterID string) error {
	if err := packstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := packstore.ValidateCollection(collection); err != nil {
		return err
	}
	if limit == 0 {
		return packstore.ValidateDocID(id)
	}
	if limit < 1 || limit > packstore.MaxListLimit {
		return packstore.ErrInvalidKey
	}
	if afterID != "" {
		return packstore.ValidateDocID(afterID)
	}
	return nil
}
