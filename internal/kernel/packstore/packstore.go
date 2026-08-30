// Package packstore 是包持久化契约的内核窄端口：沙箱 guest 经通用宿主函数
// ailuo.store 读写宿主统一存储，作用域强制为（App × 包 namespace）。
//
// 内核只实现一次该端口与 ABI；任何包需要持久化时在清单 [storage] 段声明
// namespace 并声明 ailuo.store 宿主函数，装载期 fail-closed 校验"声明 ⊆ 授权"。
// 快照原子替换（ReplaceSnapshot）只对可信 Go 侧开放，不进入 guest ABI：
// 快照导入校验与原子性是内核特权，guest 只做读取期的新鲜度/权威性治理。
package packstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
)

// 通用存储的闭式资源上限：guest 侧每次调用与可信侧快照导入共用，
// 超限一律显式错误，不做静默截断。
const (
	// MaxPayloadBytes 是单文档载荷字节上限。
	MaxPayloadBytes = 64 << 10
	// MaxListLimit 是单次 list 返回文档数上限。
	MaxListLimit = 200
	// MaxSnapshotDocs 是单集合快照文档数上限。
	MaxSnapshotDocs = 100_000
	// MaxSnapshotCollections 是单次快照替换的集合数上限。
	MaxSnapshotCollections = 32
)

var (
	// ErrNotFound 表示文档或快照元数据不存在。
	ErrNotFound = errors.New("package document not found")
	// ErrInvalidScope 表示存储作用域非法（App/namespace 未通过闭式校验）。
	ErrInvalidScope = errors.New("invalid package storage scope")
	// ErrInvalidKey 表示集合名或文档 ID 非法。
	ErrInvalidKey = errors.New("invalid package storage key")
	// ErrInvalidPayload 表示文档载荷非法（非 JSON 或超限）。
	ErrInvalidPayload = errors.New("invalid package document payload")
	// ErrInvalidSnapshot 表示快照元数据或内容未通过导入校验。
	ErrInvalidSnapshot = errors.New("invalid package snapshot")
)

// Scope 是一次包存储访问的强制作用域：AppID 来自治理上下文（guest 不可见、
// 不可伪造），Namespace 来自包清单 [storage] 声明（guest 不可选择）。两者都
// 由宿主侧装配，guest ABI 调用无法触达其他作用域。
type Scope struct {
	AppID     string
	Namespace string
}

// Document 是一条包文档：稳定 ID + JSON 载荷。JSON 标签是 ailuo.store ABI
// 的传输形状（guest 按此解析），字段名与标签必须一致保持。
type Document struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"doc"`
}

// SnapshotMeta 是包 namespace 的数据快照治理元数据：来源修订、权威性、
// 导入时间与有效期。语义与 packagecontract.Storage 声明对应，guest 读取后
// 自行执行新鲜度/权威性治理（AGENTS.md 数据规则）。
type SnapshotMeta struct {
	Revision      string    `json:"source_revision"`
	Source        string    `json:"source"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	ImportedAt    time.Time `json:"imported_at"`
	ValidUntil    time.Time `json:"valid_until"`
}

// CollectionRead 是一次集合读取的一致性视图：Documents 与 Meta 来自同一
// 读取事务（快照原子替换下 reader 永不观察到跨修订混合）。MetaFound 为
// false 表示 namespace 尚无生效快照（文档可能由 put 独立写入）。
type CollectionRead struct {
	Meta      SnapshotMeta
	MetaFound bool
	Documents []Document
}

// DocumentRead 是一次单文档读取的一致性视图，语义同 CollectionRead。
type DocumentRead struct {
	Meta      SnapshotMeta
	MetaFound bool
	Found     bool
	Document  Document
}

// Store 是包文档存储的窄端口。实现负责按 Scope 强制 App 隔离、参数化查询
// 与闭式资源上限；具体引擎（SQLite/内存）位于本端口之后。读取方法必须在
// 单个读取事务内同时取回文档与其快照元数据。
type Store interface {
	// Get 返回单文档读取视图；不存在时 Found 为 false。
	Get(ctx context.Context, scope Scope, collection, id string) (DocumentRead, error)
	// Put 以 upsert 语义写入单文档。
	Put(ctx context.Context, scope Scope, collection, id string, payload json.RawMessage) error
	// Delete 删除单文档；不存在时返回 ErrNotFound。
	Delete(ctx context.Context, scope Scope, collection, id string) error
	// List 按 doc_id 升序返回集合文档，afterID 为空表示从头开始；limit 必须在
	// 1..MaxListLimit。返回数量可能小于 limit，但不做跨调用游标语义。
	List(ctx context.Context, scope Scope, collection string, limit int, afterID string) (CollectionRead, error)
	// ReplaceSnapshot 原子替换 namespace 的全部集合并激活新快照元数据：
	// 任一校验失败保留上一完整版本（AGENTS.md 快照导入规则）。仅可信 Go 侧调用。
	ReplaceSnapshot(ctx context.Context, scope Scope, meta SnapshotMeta, collections map[string][]Document) error
}

// ValidateScope 校验作用域：AppID 与 namespace 均为闭式标识。
func ValidateScope(scope Scope) error {
	if !capability.IsStableID(scope.AppID) {
		return ErrInvalidScope
	}
	if packagecontract.ValidateStorage(packagecontract.Storage{
		Namespace: scope.Namespace, SchemaVersion: 1,
		Sensitivity: packagecontract.SensitivityPublic, Retention: packagecontract.RetentionPermanent,
	}) != nil {
		return ErrInvalidScope
	}
	return nil
}

// ValidateCollection 校验集合名（复用稳定标识闭式规则）。
func ValidateCollection(collection string) error {
	if !capability.IsStableID(collection) {
		return ErrInvalidKey
	}
	return nil
}

// ValidateDocID 校验文档 ID。
func ValidateDocID(id string) error {
	if !capability.IsStableID(id) {
		return ErrInvalidKey
	}
	return nil
}

// ValidatePayload 校验载荷：必须为合法 UTF-8 JSON 且不超限。
func ValidatePayload(payload json.RawMessage) error {
	if len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return ErrInvalidPayload
	}
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return ErrInvalidPayload
	}
	return nil
}

// ValidateSnapshotMeta 校验快照元数据：完整、来源明确、时间有序。
func ValidateSnapshotMeta(meta SnapshotMeta) error {
	if !capability.IsStableID(meta.Revision) || meta.Source == "" || len(meta.Source) > 256 ||
		!utf8.ValidString(meta.Source) || !meta.Complete || meta.ImportedAt.IsZero() ||
		meta.ValidUntil.IsZero() || !meta.ValidUntil.After(meta.ImportedAt) {
		return ErrInvalidSnapshot
	}
	return nil
}

// ValidateSnapshot 校验一次快照导入的完整内容：元数据、集合数量、每集合
// 文档数量、键合法性与载荷唯一性。任何失败都使导入整体被拒。
func ValidateSnapshot(meta SnapshotMeta, collections map[string][]Document) error {
	if err := ValidateSnapshotMeta(meta); err != nil {
		return err
	}
	if len(collections) == 0 || len(collections) > MaxSnapshotCollections {
		return ErrInvalidSnapshot
	}
	for collection, documents := range collections {
		if err := ValidateCollection(collection); err != nil {
			return fmt.Errorf("%w: collection %q: %v", ErrInvalidSnapshot, collection, err)
		}
		if len(documents) == 0 || len(documents) > MaxSnapshotDocs {
			return fmt.Errorf("%w: collection %q has %d documents", ErrInvalidSnapshot, collection, len(documents))
		}
		seen := make(map[string]struct{}, len(documents))
		for _, document := range documents {
			if err := ValidateDocID(document.ID); err != nil {
				return fmt.Errorf("%w: collection %q id %q: %v", ErrInvalidSnapshot, collection, document.ID, err)
			}
			if err := ValidatePayload(document.Payload); err != nil {
				return fmt.Errorf("%w: collection %q document %q: %v", ErrInvalidSnapshot, collection, document.ID, err)
			}
			if _, duplicate := seen[document.ID]; duplicate {
				return fmt.Errorf("%w: collection %q duplicate document %q", ErrInvalidSnapshot, collection, document.ID)
			}
			seen[document.ID] = struct{}{}
		}
	}
	return nil
}
