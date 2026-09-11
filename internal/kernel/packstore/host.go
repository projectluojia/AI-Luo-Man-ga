package packstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader/wasmhost"
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

// ScopeRequest 是 ailuo.store 请求信封的作用域选择：guest 只能声明要访问
// 系统数据还是个人数据，具体 UserID 由宿主从治理上下文注入，guest 不可见、
// 不可伪造。缺省是 system（公共快照数据）。
type ScopeRequest struct {
	Scope ScopeKind `json:"scope,omitempty"`
}

// scopeFor 解析请求声明的作用域种类并绑定归属用户：system 固定空 UserID，
// user 取治理上下文的 UserID（缺失即拒绝——匿名调用没有个人数据）。
func scopeFor(request contracts.RequestContext, declared ScopeKind) (Scope, error) {
	switch declared {
	case ScopeSystem, "":
		return Scope{AppID: request.AppID, UserID: ""}, nil
	case ScopeUser:
		if request.UserID == "" {
			return Scope{}, fmt.Errorf("%w: user scope requires an authenticated caller", ErrInvalidScope)
		}
		return Scope{AppID: request.AppID, UserID: request.UserID}, nil
	default:
		return Scope{}, fmt.Errorf("%w: unknown storage scope kind %q", ErrInvalidScope, declared)
	}
}

type getRequest struct {
	ScopeRequest
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
	ScopeRequest
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
	ScopeRequest
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Doc        json.RawMessage `json:"doc"`
}

type deleteRequest struct {
	ScopeRequest
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type deleteResponse struct {
	Deleted bool `json:"deleted"`
}

// HostFunctions 返回绑定到固定 Package namespace 的通用存储宿主函数。
// AppID 与 UserID 取自每次调用的治理上下文（宿主侧注入，guest 不可伪造），
// PackageID 与 namespace 由装配方固定；guest 只能声明作用域种类（system/user）。
// 写操作必须对应声明为 write/external 的 Capability、携带幂等键，且只能写
// 个人作用域——公共数据只经可信 Go 侧快照导入，guest 无系统级写入路径。
func HostFunctions(store Store, packageID, namespace string, capabilities []capability.CapabilitySpec) []wasmhost.HostedFunction {
	capabilityByID := make(map[string]capability.CapabilitySpec, len(capabilities))
	for _, spec := range capabilities {
		capabilityByID[spec.ID] = spec
	}
	storeBinding := func(operation string, handler func(context.Context, contracts.RequestContext, []byte) (any, error)) func(context.Context, contracts.RequestContext, []byte) ([]byte, error) {
		return func(ctx context.Context, request contracts.RequestContext, body []byte) ([]byte, error) {
			if err := authorizeStoreOperation(request, operation, capabilityByID); err != nil {
				return nil, err
			}
			response, err := handler(ctx, request, body)
			if err != nil {
				return nil, err
			}
			return json.Marshal(response)
		}
	}
	// bindScope 从请求声明的作用域种类与治理上下文装配完整作用域：UserID
	// 只能来自治理上下文；写操作只能落个人作用域（防止任一用户经 guest 路径
	// 污染全体用户共享的系统数据）。
	bindScope := func(request contracts.RequestContext, operation string, declared ScopeKind) (Scope, error) {
		partial, err := scopeFor(request, declared)
		if err != nil {
			return Scope{}, err
		}
		if (operation == OpPut || operation == OpDelete) && partial.Kind() != ScopeUser {
			return Scope{}, fmt.Errorf("%w: %s requires user scope", ErrAccessDenied, operation)
		}
		scope := Scope{
			AppID: partial.AppID, UserID: partial.UserID,
			PackageID: packageID, Namespace: namespace,
		}
		if err := ValidateScope(scope); err != nil {
			return Scope{}, err
		}
		return scope, nil
	}
	decode := func(body []byte, target any) error {
		if err := packagecontract.DecodeStrictJSON(body, target); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidKey, err)
		}
		return nil
	}
	return []wasmhost.HostedFunction{
		{
			Module: StoreModule, Name: OpGet,
			Call: storeBinding(OpGet, func(ctx context.Context, request contracts.RequestContext, body []byte) (any, error) {
				var req getRequest
				if err := decode(body, &req); err != nil {
					return nil, err
				}
				scope, err := bindScope(request, OpGet, req.Scope)
				if err != nil {
					return nil, err
				}
				if err := ValidateCollection(req.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(req.ID); err != nil {
					return nil, err
				}
				read, err := store.Get(ctx, scope, req.Collection, req.ID)
				if err != nil {
					return nil, err
				}
				return getResponse{Found: read.Found, Doc: read.Document.Payload, Meta: read.Meta, MetaFound: read.MetaFound}, nil
			}),
		},
		{
			Module: StoreModule, Name: OpList,
			Call: storeBinding(OpList, func(ctx context.Context, request contracts.RequestContext, body []byte) (any, error) {
				var req listRequest
				if err := decode(body, &req); err != nil {
					return nil, err
				}
				scope, err := bindScope(request, OpList, req.Scope)
				if err != nil {
					return nil, err
				}
				if err := ValidateCollection(req.Collection); err != nil {
					return nil, err
				}
				if req.Limit < 1 || req.Limit > MaxListLimit {
					return nil, ErrInvalidKey
				}
				if req.AfterID != "" {
					if err := ValidateDocID(req.AfterID); err != nil {
						return nil, err
					}
				}
				read, err := store.List(ctx, scope, req.Collection, req.Limit, req.AfterID)
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
			Call: storeBinding(OpPut, func(ctx context.Context, request contracts.RequestContext, body []byte) (any, error) {
				var req putRequest
				if err := decode(body, &req); err != nil {
					return nil, err
				}
				scope, err := bindScope(request, OpPut, req.Scope)
				if err != nil {
					return nil, err
				}
				if err := ValidateCollection(req.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(req.ID); err != nil {
					return nil, err
				}
				if err := ValidatePayload(req.Doc); err != nil {
					return nil, err
				}
				return struct{}{}, store.Put(ctx, scope, req.Collection, req.ID, req.Doc)
			}),
		},
		{
			Module: StoreModule, Name: OpDelete,
			Call: storeBinding(OpDelete, func(ctx context.Context, request contracts.RequestContext, body []byte) (any, error) {
				var req deleteRequest
				if err := decode(body, &req); err != nil {
					return nil, err
				}
				scope, err := bindScope(request, OpDelete, req.Scope)
				if err != nil {
					return nil, err
				}
				if err := ValidateCollection(req.Collection); err != nil {
					return nil, err
				}
				if err := ValidateDocID(req.ID); err != nil {
					return nil, err
				}
				if err := store.Delete(ctx, scope, req.Collection, req.ID); err != nil {
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

func authorizeStoreOperation(request contracts.RequestContext, operation string, capabilities map[string]capability.CapabilitySpec) error {
	spec, ok := capabilities[request.CapabilityID]
	if !ok {
		return ErrAccessDenied
	}
	if spec.Execution.EffectTarget != capability.EffectState && spec.Execution.EffectTarget != capability.EffectExternal {
		// EffectNone 承诺无副作用：读可以放行，写（Put/Delete）一律拒绝，
		// 不因幂等键/确认满足而破例。
		if operation == OpPut || operation == OpDelete {
			return ErrAccessDenied
		}
		return nil
	}
	if spec.Execution.Replay != capability.ReplayIdempotencyKey || request.IdempotencyKey == "" ||
		(spec.Execution.ConfirmationFloor == capability.ConfirmationRequired && request.ConfirmationID == "") {
		return ErrAccessDenied
	}
	return nil
}

// ManifestFunctions 是装配期宿主函数提供者：按包清单返回绑定到该包
// namespace 的存储宿主函数。声明了 ailuo.store.* 却未声明 [storage] 的清单
// 直接拒绝（fail-closed）；未声明存储函数的包返回空集。
func ManifestFunctions(store Store, manifest loader.Manifest) ([]wasmhost.HostedFunction, error) {
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
	if manifest.Storage == nil || manifest.PackageID == "" {
		return nil, fmt.Errorf("%w: package %q imports %s without declaring storage namespace",
			loader.ErrInvalidManifest, manifest.ID, StoreModule)
	}
	if packagecontract.ValidateStorage(*manifest.Storage) != nil {
		return nil, fmt.Errorf("%w: package %q storage declaration is invalid",
			loader.ErrInvalidManifest, manifest.ID)
	}
	return HostFunctions(store, manifest.PackageID, manifest.Storage.Namespace, manifest.Capabilities), nil
}
