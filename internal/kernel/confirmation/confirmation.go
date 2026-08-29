// Package confirmation 实现"确认与副作用治理"的权威模型与存储端口。
//
// 确认（Confirmation）是 Go 内核持有的、针对高风险副作用动作（发送消息、
// 写入记忆、修改数据、调用外部系统）的持久化授权凭证。确认记录创建时绑定
// App、Echo、会话、Run、Call、Capability/Tool、幂等键与参数摘要；验证放行
// 的强制绑定为 App、目标、副作用、参数摘要与会话归属（同 Echo 或同会话，
// 支持决策后跨 Echo 续跑；Run/Call/幂等键仅作审计溯源），只有 approved 且
// 全部强制维度匹配的记录才能放行副作用执行，其余状态一律 fail-closed。
//
// 本包不拥有任何具体数据库实现：领域与调度代码只依赖 Store 端口，
// 具体适配器（SQLite 等）在 internal/storage 下实现。
package confirmation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

// 确认状态机。waiting 是唯一可决策状态；approved 是唯一可执行副作用的授权态；
// rejected / expired / revoked 均为不可逆转的失效状态。
const (
	StatusWaiting  = "waiting"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
	StatusRevoked  = "revoked"
)

// 确认目标类型与副作用类型复用 Registry 的闭式取值，避免双份常量漂移。
const (
	TargetTypeCapability = registry.TargetTypeCapability
	TargetTypeTool       = registry.TargetTypeTool
	SideEffectWrite      = registry.SideEffectWrite
	SideEffectExternal   = registry.SideEffectExternal
)

// DefaultLifetime 是未显式指定有效期时待确认记录的默认有效时长。
const DefaultLifetime = 30 * time.Minute

const (
	maxIDBytes       = 256
	maxArgumentBytes = 256 << 10 // 参数摘要入参上限：256 KiB
	digestHexLength  = sha256.Size * 2
)

// 稳定错误：跨进程、跨版本保持语义不变，公共接口与调用方据此判定。
var (
	ErrInvalidRequest    = errors.New("invalid confirmation request")
	ErrNotFound          = errors.New("confirmation not found")
	ErrNotApproved       = errors.New("confirmation is not approved")
	ErrExpired           = errors.New("confirmation has expired")
	ErrRevoked           = errors.New("confirmation has been revoked")
	ErrAlreadyDecided    = errors.New("confirmation was already decided")
	ErrScopeMismatch     = errors.New("confirmation scope mismatch")
	ErrDuplicate         = errors.New("confirmation already exists")
	ErrInvalidTransition = errors.New("invalid confirmation state transition")
)

var (
	stableIDPattern = id.StableMixedUncapped // 长度上限由 maxIDBytes（256）单独校验
	digestPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Confirmation 是一条持久化的确认记录。
// App、Echo、Run、Call、目标、幂等键与参数摘要共同构成确认的绑定范围，
// 任何维度不匹配都不得放行副作用执行。
type Confirmation struct {
	AppID          string     // 所属 App，App 隔离的第一维度
	ConfirmationID string     // App 内唯一的确认标识
	EchoID         string     // 发起确认的 Echo
	SessionID      string     // 发起确认的会话（接入层 Intake 权威生成）；跨 Echo 重试的范围锚点
	RunID          string     // 发起确认的 Run
	CallID         string     // 发起确认的调用
	CapabilityID   string     // 目标 Capability（tool 目标可为空）
	TargetType     string     // 目标类型：capability / tool
	TargetID       string     // 目标标识（Capability 或 Tool ID）
	SideEffect     string     // 副作用类型：write / external
	IdempotencyKey string     // 与执行期一致的幂等键
	ArgumentDigest string     // 确认时参数摘要（规范化 JSON 的 sha256 十六进制）
	Status         string     // 状态：waiting/approved/rejected/expired/revoked
	ExpiresAt      time.Time  // 有效期截止时间，过期后一律失效
	ConfirmedBy    string     // 批准/拒绝的决策人标识
	DecidedAt      *time.Time // 最近一次状态决策时间
	CreatedAt      time.Time  // 创建时间
}

// RequestSpec 描述一条待确认请求的目标与副作用绑定。
type RequestSpec struct {
	CapabilityID   string // 目标 Capability；target_type=capability 时缺省取 target_id
	TargetType     string // capability 或 tool
	TargetID       string // Capability 或 Tool 标识
	SideEffect     string // write 或 external
	IdempotencyKey string // 与执行期相同的幂等键
	SessionID      string // 发起确认的会话标识；非空时决策后的重试可在同会话的新 Echo 中进行
}

// Store 是确认记录的持久化端口。领域与调度代码只依赖该端口，
// 具体数据库适配（SQLite 等）实现它；所有读写都必须按 App 作用域执行。
type Store interface {
	// Create 持久化一条新的待确认记录；同主键已存在返回 ErrDuplicate。
	Create(ctx context.Context, record Confirmation) error

	// Get 按 (app_id, confirmation_id) 读取确认记录；不存在返回 ErrNotFound。
	Get(ctx context.Context, appID, confirmationID string) (Confirmation, error)

	// Decide 以 CAS 方式把 waiting 状态的确认原子决策为 approved/rejected。
	// 转换成功返回 (当前记录, true)；状态已变化或有效期已过返回 (当前记录, false)，
	// 由调用方依据当前状态判定幂等或冲突语义。
	Decide(ctx context.Context, appID, confirmationID, status, confirmedBy string, decidedAt time.Time) (Confirmation, bool, error)

	// Revoke 以 CAS 方式撤销一条 waiting/approved 且未过有效期的确认。
	// 转换成功返回 (当前记录, true)，否则返回 (当前记录, false)。
	Revoke(ctx context.Context, appID, confirmationID string, revokedAt time.Time) (Confirmation, bool, error)

	// Expire 以 CAS 方式把一条 waiting/approved 的确认标记为过期（可提前强制过期）。
	// 转换成功返回 (当前记录, true)，否则返回 (当前记录, false)。
	Expire(ctx context.Context, appID, confirmationID string, expiredAt time.Time) (Confirmation, bool, error)

	// ExpireDue 批量过期指定 App 下所有已到期但仍 waiting/approved 的确认，
	// 返回受影响行数。
	ExpireDue(ctx context.Context, appID string, now time.Time) (int64, error)

	// RevokeRun 撤销指定 App 的 Run 下所有 waiting/approved 确认（Run 取消场景），
	// 返回受影响行数。
	RevokeRun(ctx context.Context, appID, runID string, now time.Time) (int64, error)

	// ListActiveByEcho 返回指定 App、Echo 下仍未失效（waiting/approved 且
	// 有效期未过）的确认记录，按创建时间升序，供 Run 启动投影与状态查询。
	ListActiveByEcho(ctx context.Context, appID, echoID string, now time.Time) ([]Confirmation, error)

	// RevokeEcho 撤销指定 App、Echo 下所有 waiting/approved 确认（Echo 取消场景），
	// 返回受影响行数。
	RevokeEcho(ctx context.Context, appID, echoID string, now time.Time) (int64, error)

	// ListActiveBySession 返回指定 App、会话下仍未失效（waiting/approved 且
	// 有效期未过）的确认记录，按创建时间升序，供跨 Echo 续跑投影使用。
	ListActiveBySession(ctx context.Context, appID, sessionID string, now time.Time) ([]Confirmation, error)

	// FindApproved 按（目标类型、目标标识、参数摘要、会话归属）解析唯一可用的
	// 已批准确认：归属匹配同 Echo 或同会话，有效期未过，取最新一条；不存在
	// 返回 ErrNotFound。供内核在调用边界按参数自选正确批准，不依赖执行者
	// 附带的 confirmation_id。
	FindApproved(ctx context.Context, appID, echoID, sessionID, targetType, targetID, digest string, now time.Time) (Confirmation, error)
}

// Digest 计算确认参数的规范化摘要：把参数反序列化后重新序列化（JSON 键有序），
// 再取 sha256。空参数等价于空对象 {}。参数改变后摘要必然改变，
// 使旧确认无法通过参数摘要匹配。
func Digest(arguments []byte) (string, error) {
	if len(arguments) > maxArgumentBytes {
		return "", fmt.Errorf("%w: arguments exceed %d bytes", ErrInvalidRequest, maxArgumentBytes)
	}
	sum, err := jsonutil.CanonicalDigest(arguments)
	if err != nil {
		return "", fmt.Errorf("%w: arguments are not valid JSON", ErrInvalidRequest)
	}
	return hex.EncodeToString(sum[:]), nil
}

// ValidateStatus 校验确认状态值。
func ValidateStatus(value string) error {
	switch value {
	case StatusWaiting, StatusApproved, StatusRejected, StatusExpired, StatusRevoked:
		return nil
	default:
		return fmt.Errorf("%w: invalid status %q", ErrInvalidRequest, value)
	}
}

// ValidateTargetType 校验确认目标类型。
func ValidateTargetType(value string) error {
	switch value {
	case TargetTypeCapability, TargetTypeTool:
		return nil
	default:
		return fmt.Errorf("%w: invalid target type %q", ErrInvalidRequest, value)
	}
}

// ValidateSideEffect 校验确认适用的副作用类型。
func ValidateSideEffect(value string) error {
	switch value {
	case SideEffectWrite, SideEffectExternal:
		return nil
	default:
		return fmt.Errorf("%w: invalid side effect %q", ErrInvalidRequest, value)
	}
}

// ValidateArgumentDigest 校验参数摘要格式（64 位十六进制）。
func ValidateArgumentDigest(value string) error {
	if len(value) != digestHexLength || !digestPattern.MatchString(value) {
		return fmt.Errorf("%w: invalid argument digest", ErrInvalidRequest)
	}
	return nil
}

// ValidateRequest 校验 Dispatcher 注入的确认验证请求字段。
// ArgumentDigest 由 Dispatcher 边界按与 Digest 相同的算法计算，必填。
func ValidateRequest(request runtime.ConfirmationRequest) error {
	switch {
	case request.AppID == "" || request.EchoID == "" || request.RunID == "" || request.ConfirmationID == "":
		return ErrInvalidRequest
	case len(request.ConfirmationID) > maxIDBytes:
		return ErrInvalidRequest
	case ValidateTargetType(request.TargetType) != nil || request.TargetID == "" || len(request.TargetID) > maxIDBytes:
		return ErrInvalidRequest
	case ValidateSideEffect(request.SideEffect) != nil:
		return ErrInvalidRequest
	case idempotency.ValidateKey(request.IdempotencyKey) != nil:
		return ErrInvalidRequest
	case ValidateArgumentDigest(request.ArgumentDigest) != nil:
		return ErrInvalidRequest
	default:
		return nil
	}
}

// ValidateConfirmation 校验一条完整确认记录的字段、状态一致性与时间约束。
func ValidateConfirmation(record Confirmation) error {
	if !validID(record.AppID) || !validID(record.ConfirmationID) || !validID(record.EchoID) ||
		!validID(record.RunID) || !validID(record.CallID) {
		return ErrInvalidRequest
	}
	if record.CapabilityID != "" && !validID(record.CapabilityID) {
		return ErrInvalidRequest
	}
	if record.SessionID != "" && (len(record.SessionID) > maxIDBytes || !utf8.ValidString(record.SessionID)) {
		return ErrInvalidRequest
	}
	if ValidateTargetType(record.TargetType) != nil || !validID(record.TargetID) {
		return ErrInvalidRequest
	}
	if ValidateSideEffect(record.SideEffect) != nil || idempotency.ValidateKey(record.IdempotencyKey) != nil {
		return ErrInvalidRequest
	}
	if ValidateArgumentDigest(record.ArgumentDigest) != nil {
		return ErrInvalidRequest
	}
	if ValidateStatus(record.Status) != nil {
		return ErrInvalidRequest
	}
	if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return ErrInvalidRequest
	}
	// 状态一致性：waiting 无决策时间；其余状态必须携带决策时间。
	if (record.Status == StatusWaiting && record.DecidedAt != nil) ||
		(record.Status != StatusWaiting && (record.DecidedAt == nil || record.DecidedAt.IsZero())) {
		return ErrInvalidRequest
	}
	// 批准/拒绝必须记录决策人。
	if (record.Status == StatusApproved || record.Status == StatusRejected) && record.ConfirmedBy == "" {
		return ErrInvalidRequest
	}
	return nil
}

func validID(value string) bool {
	return value != "" && len(value) <= maxIDBytes && utf8.ValidString(value) && stableIDPattern.MatchString(value)
}
