// Package guestkit 是 hosted wasm guest 的共享基础库：调用信封、ailuo.store
// 线性内存 ABI、集合分页读取与快照治理。全部仅依赖标准库，guest 侧不得在此
// 之外再实现第二份同类逻辑——新增 guest 能力一律先复用本包。
//
// 本包只在 wasip1 guest 内编译使用；宿主函数通过 go:wasmimport 由各 guest
// 按清单声明的子集注入，本包提供声明桩（get/list/put/delete）与统一调用封装。
package guestkit

import (
	"encoding/json"
	"errors"
	"time"
	"unsafe"
)

// 宿主函数调用失败的长度标记（-1 的无符号表示）与响应缓冲上限（与内核
// 消息上限一致）。
const (
	HostFunctionError = 0xFFFFFFFF
	MaxStoreResponse  = 512 << 10
)

// 稳定错误码（内核闭式集合：guest 自定义码会被内核视为协议违例）。
const (
	CodeInvalidArgument = "invalid_argument"
	CodeInternal        = "internal"
	CodeDataUnavailable = "data_unavailable"
	CodeDataIncomplete  = "data_incomplete"
	CodeDataUntrusted   = "data_untrusted"
	CodeDataExpired     = "data_expired"
)

// 作用域种类（与宿主 ailuo.store ABI 的 scope 字段一致）：缺省 system 是
// 公共快照数据（只读），user 是个人作用域（UserID 由宿主从治理上下文注入，
// guest 不可指定、不可伪造）。写操作（put/delete）只能落 user 作用域。
type Scope string

const (
	ScopeSystem Scope = "system"
	ScopeUser   Scope = "user"
)

// RequestEnvelope 与宿主约定的 stdin 调用信封。
type RequestEnvelope struct {
	CapabilityID string          `json:"capability_id"`
	Payload      json.RawMessage `json:"payload"`
}

// ResultEnvelope 与宿主约定的 stdout 结果信封。
type ResultEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// SnapshotMeta 是快照治理元数据（系统作用域读取响应内嵌，与本次读取同源）。
type SnapshotMeta struct {
	SourceRevision string    `json:"source_revision"`
	Source         string    `json:"source"`
	Authoritative  bool      `json:"authoritative"`
	Complete       bool      `json:"complete"`
	ImportedAt     time.Time `json:"imported_at"`
	ValidUntil     time.Time `json:"valid_until"`
}

// Document 是 ailuo.store 的文档传输形状。
type Document struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"doc"`
}

// get 请求与响应（宿主函数 ailuo.store get 的 ABI 信封）。
type storeGetRequest struct {
	Scope      Scope  `json:"scope,omitempty"`
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type storeGetResponse struct {
	Found     bool            `json:"found"`
	Doc       json.RawMessage `json:"doc,omitempty"`
	Meta      SnapshotMeta    `json:"meta"`
	MetaFound bool            `json:"meta_found"`
}

// list 请求与响应。
type storeListRequest struct {
	Scope      Scope  `json:"scope,omitempty"`
	Collection string `json:"collection"`
	Limit      int    `json:"limit"`
	AfterID    string `json:"after_id,omitempty"`
}

// put 请求（宿主函数 ailuo.store put 的 ABI 信封）。
type storePutRequest struct {
	Scope      Scope           `json:"scope,omitempty"`
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Doc        json.RawMessage `json:"doc"`
}

// delete 请求与响应。
type storeDeleteRequest struct {
	Scope      Scope  `json:"scope,omitempty"`
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type storeDeleteResponse struct {
	Deleted bool `json:"deleted"`
}

// StoreClient 是 ailuo.store 宿主函数的 guest 侧封装：一个 guest 进程
// 一份，按清单声明的宿主函数子集调用。
type StoreClient struct{}

// NewStoreClient 构造存储客户端（无状态占位，保持调用点可读）。
func NewStoreClient() *StoreClient { return &StoreClient{} }

// call 以线性内存 ABI 调用一个宿主函数；返回 nil 表示宿主侧失败。
func call(request []byte, importFn func(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32) []byte {
	if len(request) == 0 {
		request = []byte("{}")
	}
	response := make([]byte, MaxStoreResponse)
	length := importFn(unsafe.Pointer(&request[0]), uint32(len(request)), unsafe.Pointer(&response[0]), uint32(len(response)))
	if length == HostFunctionError {
		return nil
	}
	return response[:length]
}

// Get 读取单个文档（默认系统作用域；个人作用域传 ScopeUser）。
// found 为 false 表示文档不存在（或个人作用域下不属于本人）。
func (c *StoreClient) Get(scope Scope, collection, id string) (payload json.RawMessage, found bool, err error) {
	request, err := json.Marshal(storeGetRequest{Scope: scope, Collection: collection, ID: id})
	if err != nil {
		return nil, false, err
	}
	response := call(request, storeGet)
	if response == nil {
		return nil, false, errors.New("store get call failed")
	}
	var decoded storeGetResponse
	if err := json.Unmarshal(response, &decoded); err != nil {
		return nil, false, err
	}
	return decoded.Doc, decoded.Found, nil
}

// ListPage 是一次分页读取：文档与快照元数据同源（宿主单事务读出）。
type ListPage struct {
	Docs      []Document
	Meta      SnapshotMeta
	MetaFound bool
}

// List 读取一页文档（默认系统作用域）。
func (c *StoreClient) List(scope Scope, collection string, limit int, afterID string) (ListPage, error) {
	request, err := json.Marshal(storeListRequest{Scope: scope, Collection: collection, Limit: limit, AfterID: afterID})
	if err != nil {
		return ListPage{}, err
	}
	response := call(request, storeList)
	if response == nil {
		return ListPage{}, errors.New("store list call failed")
	}
	var page ListPage
	if err := json.Unmarshal(response, &page); err != nil {
		return ListPage{}, err
	}
	if page.Docs == nil {
		page.Docs = []Document{}
	}
	return page, nil
}

// Put 以 upsert 语义写个人作用域文档。系统作用域写入没有 guest 路径，
// 传 ScopeSystem 会被宿主拒绝。
func (c *StoreClient) Put(collection, id string, doc json.RawMessage) error {
	request, err := json.Marshal(storePutRequest{Scope: ScopeUser, Collection: collection, ID: id, Doc: doc})
	if err != nil {
		return err
	}
	if call(request, storePut) == nil {
		return errors.New("store put call failed")
	}
	return nil
}

// Delete 删除个人作用域文档；宿主对不存在的文档返回 deleted=false。
func (c *StoreClient) Delete(collection, id string) (bool, error) {
	request, err := json.Marshal(storeDeleteRequest{Scope: ScopeUser, Collection: collection, ID: id})
	if err != nil {
		return false, err
	}
	response := call(request, storeDelete)
	if response == nil {
		return false, errors.New("store delete call failed")
	}
	var decoded storeDeleteResponse
	if err := json.Unmarshal(response, &decoded); err != nil {
		return false, err
	}
	return decoded.Deleted, nil
}
