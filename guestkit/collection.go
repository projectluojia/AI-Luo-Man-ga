package guestkit

import (
	"encoding/json"
	"errors"
)

// MaxListPageSize 是单次 list 请求的页大小上限（与宿主 ailuo.store 上限一致）。
const MaxListPageSize = 200

// Lister 是集合分页读取的最小端口：由 *StoreClient 实现（任意作用域种类），
// 原生测试注入假实现。集合遍历辅助函数一律经本端口工作，禁止各包再手写
// 分页循环。
type Lister interface {
	List(scope Scope, collection string, limit int, afterID string) (ListPage, error)
}

// SnapshotCollection 是一次一致的集合读取：文档与快照元数据同源（分页期间
// 修订变化视为数据不可用，永不返回跨修订混合视图）。
type SnapshotCollection[T any] struct {
	Meta      SnapshotMeta
	MetaFound bool
	Documents []T
}

// FetchSnapshotCollection 分页读取系统作用域集合全部文档并解码到目标类型。
// 文档必须携带与快照一致的 source_revision 字段——修订不一致即数据不可用。
// decode 为 nil 时按各文档的 doc 字段直接 json.Unmarshal 解码。
func FetchSnapshotCollection[T any](source Lister, collection string, decode func(json.RawMessage) (T, error)) (SnapshotCollection[T], error) {
	var snapshot SnapshotCollection[T]
	documents := make([]T, 0, 16)
	afterID := ""
	var revision string
	hasRevision := false
	for {
		page, err := source.List(ScopeSystem, collection, MaxListPageSize, afterID)
		if err != nil {
			return snapshot, err
		}
		if hasRevision && page.Meta.SourceRevision != revision {
			return snapshot, ErrDataUnavailable
		}
		if !hasRevision {
			revision = page.Meta.SourceRevision
			hasRevision = true
		}
		for _, document := range page.Docs {
			item, err := decodeDocument(document, revision, decode)
			if err != nil {
				return snapshot, err
			}
			documents = append(documents, item)
		}
		snapshot.Meta = page.Meta
		snapshot.MetaFound = page.MetaFound
		if len(page.Docs) < MaxListPageSize {
			break
		}
		afterID = page.Docs[len(page.Docs)-1].ID
	}
	snapshot.Documents = documents
	return snapshot, nil
}

// GovernedSnapshotCollection 先分页读取再治理快照元数据，一次返回可用的
// 权威视图；治理失败返回稳定错误码错误。
func GovernedSnapshotCollection[T any](source Lister, collection string, decode func(json.RawMessage) (T, error)) (SnapshotCollection[T], error) {
	snapshot, err := FetchSnapshotCollection(source, collection, decode)
	if err != nil {
		return snapshot, err
	}
	if !snapshot.MetaFound {
		return snapshot, ErrDataUnavailable
	}
	if _, err := Govern(snapshot.Meta); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// UserCollection 分页读取个人作用域集合的全部文档并解码到目标类型。
// 个人作用域文档不携带快照元数据（宿主恒返回 MetaFound=false），无修订
// 治理语义；读取中途存储失败即整体失败，不返回部分视图。
func UserCollection[T any](source Lister, collection string, decode func(json.RawMessage) (T, error)) ([]T, error) {
	var documents []T
	afterID := ""
	for {
		page, err := source.List(ScopeUser, collection, MaxListPageSize, afterID)
		if err != nil {
			return nil, err
		}
		for _, document := range page.Docs {
			var item T
			if decode != nil {
				item, err = decode(document.Payload)
			} else {
				err = json.Unmarshal(document.Payload, &item)
			}
			if err != nil {
				return nil, err
			}
			documents = append(documents, item)
		}
		if len(page.Docs) < MaxListPageSize {
			return documents, nil
		}
		afterID = page.Docs[len(page.Docs)-1].ID
	}
}

type revisioned struct {
	SourceRevision string `json:"source_revision"`
}

// decodeDocument 校验文档修订归属并解码载荷；无自定义解码器时走标准解码。
func decodeDocument[T any](document Document, revision string, decode func(json.RawMessage) (T, error)) (T, error) {
	var zero T
	var marked revisioned
	if err := json.Unmarshal(document.Payload, &marked); err == nil && marked.SourceRevision != revision {
		return zero, ErrDataUnavailable
	}
	if decode == nil {
		var item T
		if err := json.Unmarshal(document.Payload, &item); err != nil {
			return zero, err
		}
		return item, nil
	}
	return decode(document.Payload)
}

// ErrDataUnavailable 表示集合读取失败或跨修订混合（快照元数据缺失/不一致）。
var ErrDataUnavailable = errors.New(CodeDataUnavailable)
