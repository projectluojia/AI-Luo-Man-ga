package guestkit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Store 是能力分发器对存储宿主函数的最小视图：*StoreClient 直接满足；
// 原生测试注入内存实现。系统作用域读取携带快照治理元数据，个人作用域
// 写入按治理上下文注入的 UserID 隔离。
type Store interface {
	Get(scope Scope, collection, id string) (json.RawMessage, bool, error)
	List(scope Scope, collection string, limit int, afterID string) (ListPage, error)
	Put(collection, id string, doc json.RawMessage) error
	Delete(collection, id string) (bool, error)
}

// Dispatcher 把能力载荷分发给注册的处理函数，并把结果规整为 ResultEnvelope：
// 处理函数返回错误时按 CodeFor 映射稳定错误码，返回值统一 json 编码。
// 业务失败（not_found/conflict 等）不走 ok:false——在 ok:true 的载荷内带内表达。
type Dispatcher struct {
	handlers map[string]func(payload json.RawMessage) (any, error)
}

// NewDispatcher 构造分发器。handlers 的键是能力标识（与清单 exports 一致）。
func NewDispatcher(handlers map[string]func(payload json.RawMessage) (any, error)) *Dispatcher {
	copied := make(map[string]func(payload json.RawMessage) (any, error), len(handlers))
	for id, handler := range handlers {
		copied[id] = handler
	}
	return &Dispatcher{handlers: copied}
}

// Dispatch 是 guestkit.Run 的分发入口：返回值始终是已编码的 ResultEnvelope。
func (d *Dispatcher) Dispatch(capabilityID string, payload json.RawMessage) ResultEnvelope {
	handler, known := d.handlers[capabilityID]
	if !known {
		// 未注册的 capability 属协议错误（内核不会放行未导出的调用），
		// 按稳定 invalid_argument 应答。
		return ResultEnvelope{OK: false, Code: CodeInvalidArgument, Message: "capability is not registered"}
	}
	result, err := handler(payload)
	if err != nil {
		return ResultEnvelope{OK: false, Code: CodeFor(err), Message: "capability failed"}
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return ResultEnvelope{OK: false, Code: CodeInternal, Message: "capability failed"}
	}
	return ResultEnvelope{OK: true, Result: data}
}

// CodeFor 把处理函数错误映射为稳定错误码：invalid_argument 哨兵与 guestkit
// 数据治理哨兵原样透传，其余一律 internal。
func CodeFor(err error) string {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return CodeInvalidArgument
	case errors.Is(err, ErrDataUnavailable):
		return CodeDataUnavailable
	case errors.Is(err, ErrDataIncomplete):
		return CodeDataIncomplete
	case errors.Is(err, ErrDataUntrusted):
		return CodeDataUntrusted
	case errors.Is(err, ErrDataExpired):
		return CodeDataExpired
	default:
		return CodeInternal
	}
}

// ErrInvalidArgument 是请求校验失败的标准哨兵：Dispatch 按 invalid_argument 应答。
// 各领域包的 NormalizeAndValidate 返回的包内哨兵应 wrap 本哨兵或在分发处映射。
var ErrInvalidArgument = errors.New("invalid argument")

// ErrInternal 是存储失败等内部错误的标准哨兵（不进映射表，一律 internal 应答）。
var ErrInternal = errors.New("internal")

// DecodeRun 解码载荷为请求类型（NormalizeAndValidate 统一边界校验）后调用
// 处理函数：能力处理函数的标准骨架，各 app 包的 Handlers 直接复用。
func DecodeRun[Q any, R any](payload json.RawMessage, handle func(Q) (R, error)) (any, error) {
	var request Q
	if err := DecodePayloadStrict(payload, &request); err != nil {
		return nil, err
	}
	if validator, ok := any(&request).(interface{ NormalizeAndValidate() error }); ok {
		if err := validator.NormalizeAndValidate(); err != nil {
			return nil, ErrInvalidArgument
		}
	}
	return handle(request)
}

// ---- 带内业务失败载荷（ok:true 内表达 not_found / conflict） ----

// NotFound 是「资源不存在」的带内载荷：found=false。
type NotFound struct {
	Found bool `json:"found"`
}

// Conflict 是「业务冲突」的带内载荷：Conflict 为机器可读的冲突种类
// （seat_conflict / quota_exceeded 等）。
type Conflict struct {
	Conflict string `json:"conflict"`
}

// DecodePayloadStrict 严格解码调用载荷（未知字段与尾随数据都是协议违例；
// len==0 视为空对象），格式错误映射为 ErrInvalidArgument。
func DecodePayloadStrict(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := DecodePayload(payload, target); err != nil {
		return ErrInvalidArgument
	}
	return nil
}

// ---- 系统快照辅助（目录/描述符类集合共用） ----

// GovernedSnapshot 读取并治理一个系统作用域快照集合，一次返回文档视图与
// 结果治理状态。载荷损坏按数据不可用归类。
func GovernedSnapshot[T any](client Store, collection string) (SnapshotCollection[T], DataStatus, error) {
	snapshot, err := GovernedSnapshotCollection(systemLister{client: client}, collection, decodeSnapshotDocument[T])
	if err != nil {
		return snapshot, DataStatus{}, err
	}
	dataStatus, err := GovernStatus(snapshot.Meta)
	if err != nil {
		return snapshot, DataStatus{}, err
	}
	return snapshot, dataStatus, nil
}

// FindSnapshotDoc 在已治理快照内按键定位文档（目录类查找共用）。
func FindSnapshotDoc[T any](snapshot SnapshotCollection[T], match func(item *T) bool) *T {
	for index := range snapshot.Documents {
		if match(&snapshot.Documents[index]) {
			return &snapshot.Documents[index]
		}
	}
	return nil
}

type systemLister struct {
	client Store
}

func (s systemLister) List(_ Scope, collection string, limit int, afterID string) (ListPage, error) {
	return s.client.List(ScopeSystem, collection, limit, afterID)
}

func decodeSnapshotDocument[T any](payload json.RawMessage) (T, error) {
	var item T
	if err := json.Unmarshal(payload, &item); err != nil {
		return item, ErrDataUnavailable
	}
	return item, nil
}

// NewDocID 生成宿主规则兼容的个人作用域文档 ID（前缀 + 随机十六进制）。
func NewDocID(prefix string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand 失败在 wasip1 内不应发生；退化为时间派生 ID。
		return prefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(buffer)
}
