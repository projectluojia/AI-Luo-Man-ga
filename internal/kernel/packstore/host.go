package packstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// StoreModule 是通用包存储宿主函数的模块名。guest 只能调用清单声明的函数，
// 未声明导入在装载期被内核拒绝（fail-closed）。
const StoreModule = "ailuo.store"

// 宿主函数名：guest 按需声明子集，读包只声明 get/list，读写包追加
// put/delete。快照元数据内嵌于读取响应，快照替换不对 guest 开放。
const (
	OpGet    = "get"
	OpList   = "list"
	OpPut    = "put"
	OpDelete = "delete"
)

type getRequest struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type getResponse struct {
	Found bool            `json:"found"`
	Doc   json.RawMessage `json:"doc,omitempty"`
	// Meta 是与本次读取一致的快照元数据（单事务读出），供 guest 做新鲜度/
	// 权威性治理；MetaFound 为 false 表示尚无生效快照。
	Meta      SnapshotMeta `json:"meta"`
	MetaFound bool         `json:"meta_found"`
}

type listRequest struct {
	Collection string `json:"collection"`
	Limit      int    `json:"limit"`
	AfterID    string `json:"after_id,omitempty"`
}

type listResponse struct {
	Docs      []Document   `json:"docs"`
	Meta      SnapshotMeta `json:"meta"`
	MetaFound bool         `json:"meta_found"`
}

type putRequest struct {
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Doc        json.RawMessage `json:"doc"`
}

type deleteRequest struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type deleteResponse struct {
	Deleted bool `json:"deleted"`
}

// HostFunctions 返回绑定到固定 namespace 的通用存储宿主函数。AppID 取自
// 每次调用的治理上下文（宿主侧注入，guest 不可伪造），namespace 由装配方
// 固定为包清单声明值——guest 无法选择作用域。
func HostFunctions(store Store, namespace string) []loader.HostedFunction {
	binding := func(call func(context.Context, Scope, []byte) (any, error)) func(context.Context, contracts.RequestContext, []byte) ([]byte, error) {
		return func(ctx context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
			scope := Scope{AppID: request.AppID, Namespace: namespace}
			if err := ValidateScope(scope); err != nil {
				return nil, err
			}
			response, err := call(ctx, scope, body)
			if err != nil {
				return nil, err
			}
			return json.Marshal(response)
		}
	}
	decode := func(body []byte, target any) error {
		if err := packagecontract.DecodeStrictJSON(body, target); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidKey, err)
		}
		return nil
	}
	return []loader.HostedFunction{
		{
			Module: StoreModule, Name: OpGet,
			Call: binding(func(ctx context.Context, scope Scope, body []byte) (any, error) {
				var request getRequest
				if err := decode(body, &request); err != nil {
					return nil, err
				}
				if err := ValidateCollection(request.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(request.ID); err != nil {
					return nil, err
				}
				read, err := store.Get(ctx, scope, request.Collection, request.ID)
				if err != nil {
					return nil, err
				}
				return getResponse{Found: read.Found, Doc: read.Document.Payload, Meta: read.Meta, MetaFound: read.MetaFound}, nil
			}),
		},
		{
			Module: StoreModule, Name: OpList,
			Call: binding(func(ctx context.Context, scope Scope, body []byte) (any, error) {
				var request listRequest
				if err := decode(body, &request); err != nil {
					return nil, err
				}
				if err := ValidateCollection(request.Collection); err != nil {
					return nil, err
				}
				if request.Limit < 1 || request.Limit > MaxListLimit {
					return nil, ErrInvalidKey
				}
				if request.AfterID != "" {
					if err := ValidateDocID(request.AfterID); err != nil {
						return nil, err
					}
				}
				read, err := store.List(ctx, scope, request.Collection, request.Limit, request.AfterID)
				if err != nil {
					return nil, err
				}
				if read.Documents == nil {
					read.Documents = []Document{}
				}
				return listResponse{Docs: read.Documents, Meta: read.Meta, MetaFound: read.MetaFound}, nil
			}),
		},
		{
			Module: StoreModule, Name: OpPut,
			Call: binding(func(ctx context.Context, scope Scope, body []byte) (any, error) {
				var request putRequest
				if err := decode(body, &request); err != nil {
					return nil, err
				}
				if err := ValidateCollection(request.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(request.ID); err != nil {
					return nil, err
				}
				if err := ValidatePayload(request.Doc); err != nil {
					return nil, err
				}
				return struct{}{}, store.Put(ctx, scope, request.Collection, request.ID, request.Doc)
			}),
		},
		{
			Module: StoreModule, Name: OpDelete,
			Call: binding(func(ctx context.Context, scope Scope, body []byte) (any, error) {
				var request deleteRequest
				if err := decode(body, &request); err != nil {
					return nil, err
				}
				if err := ValidateCollection(request.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(request.ID); err != nil {
					return nil, err
				}
				if err := store.Delete(ctx, scope, request.Collection, request.ID); err != nil {
					if errors.Is(err, ErrNotFound) {
						return deleteResponse{Deleted: false}, nil
					}
					return nil, err
				}
				return deleteResponse{Deleted: true}, nil
			}),
		},
	}
}

// ManifestFunctions 是装配期宿主函数提供者：按包清单返回绑定到该包
// namespace 的存储宿主函数。声明了 ailuo.store.* 却未声明 [storage] 的清单
// 直接拒绝（fail-closed）；未声明存储函数的包返回空集。
func ManifestFunctions(store Store, manifest loader.Manifest) ([]loader.HostedFunction, error) {
	declaredStore := false
	for _, decl := range manifest.HostFunctions {
		if decl.Module == StoreModule {
			declaredStore = true
			break
		}
	}
	if !declaredStore {
		return nil, nil
	}
	if manifest.Storage == nil {
		return nil, fmt.Errorf("%w: package %q imports %s without declaring storage namespace",
			loader.ErrInvalidManifest, manifest.ID, StoreModule)
	}
	if packagecontract.ValidateStorage(*manifest.Storage) != nil {
		return nil, fmt.Errorf("%w: package %q storage declaration is invalid",
			loader.ErrInvalidManifest, manifest.ID)
	}
	return HostFunctions(store, manifest.Storage.Namespace), nil
}
