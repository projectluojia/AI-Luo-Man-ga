package confirmation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Service 是确认治理的权威入口：创建待确认记录、原子决策、撤销、过期与验证。
// 所有方法都在持久化存储上执行，确认状态在进程重启后依然存在。
// Service 直接实现 runtime.ConfirmationVerifier，供 Dispatcher 注入。
type Service struct {
	store Store
	now   func() time.Time
}

// Option 用于在测试中注入确定性的时间来源等依赖。
type Option func(*Service)

// WithClock 覆盖服务的当前时间来源。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 基于持久化 Store 构建确认服务。
func NewService(store Store, options ...Option) *Service {
	service := &Service{store: store, now: time.Now}
	for _, option := range options {
		option(service)
	}
	return service
}

// Request 创建一个待确认记录并计算参数摘要。返回的记录携带新的确认标识，
// 调用方把它连同参数摘要一起呈现给用户做决策。expiresAt 为零时使用默认有效期。
func (s *Service) Request(
	ctx context.Context,
	appID, echoID, runID, callID string,
	spec RequestSpec,
	arguments []byte,
	expiresAt time.Time,
) (Confirmation, error) {
	now := s.now().UTC()
	digest, err := Digest(arguments)
	if err != nil {
		return Confirmation{}, err
	}
	if spec.CapabilityID == "" && spec.TargetType == TargetTypeCapability {
		spec.CapabilityID = spec.TargetID
	}
	expiry, err := effectiveExpiry(expiresAt, now)
	if err != nil {
		return Confirmation{}, err
	}
	record := Confirmation{
		AppID:          appID,
		ConfirmationID: uuid.NewString(),
		EchoID:         echoID,
		RunID:          runID,
		CallID:         callID,
		CapabilityID:   spec.CapabilityID,
		TargetType:     spec.TargetType,
		TargetID:       spec.TargetID,
		SideEffect:     spec.SideEffect,
		IdempotencyKey: spec.IdempotencyKey,
		ArgumentDigest: digest,
		Status:         StatusWaiting,
		ExpiresAt:      expiry,
		CreatedAt:      now,
	}
	if err := ValidateConfirmation(record); err != nil {
		return Confirmation{}, err
	}
	if err := s.store.Create(ctx, record); err != nil {
		return Confirmation{}, err
	}
	observe.Info(ctx, "确认已请求创建",
		observe.Component("confirmation"),
		observe.StringAttr("app_id", appID),
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("run_id", runID),
		observe.StringAttr("call_id", callID),
		observe.StringAttr("confirmation_id", record.ConfirmationID),
		observe.StringAttr("target_type", spec.TargetType),
		observe.StringAttr("target_id", spec.TargetID),
		observe.StringAttr("side_effect", spec.SideEffect),
		observe.StringAttr("status", record.Status),
		observe.StringAttr("expires_at", record.ExpiresAt.Format(time.RFC3339)),
	)
	return record, nil
}

// Decide 原子决策一条待确认记录。decision 为 approved 或 rejected，confirmedBy 为决策人。
// 批准是执行副作用的唯一授权信号；重复相同决策视为幂等成功（副作用执行由幂等层去重），
// 冲突决策返回 ErrAlreadyDecided，已过期/已撤销返回对应稳定错误。
func (s *Service) Decide(ctx context.Context, appID, confirmationID, decision, confirmedBy string, decidedAt time.Time) (Confirmation, error) {
	if decision != StatusApproved && decision != StatusRejected {
		return Confirmation{}, fmt.Errorf("%w: invalid decision %q", ErrInvalidRequest, decision)
	}
	if appID == "" || confirmationID == "" || confirmedBy == "" || decidedAt.IsZero() {
		return Confirmation{}, ErrInvalidRequest
	}
	record, transitioned, err := s.store.Decide(ctx, appID, confirmationID, decision, confirmedBy, decidedAt)
	if err != nil {
		return Confirmation{}, err
	}
	if transitioned {
		observe.Info(ctx, "确认已决策",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", appID),
			observe.StringAttr("echo_id", record.EchoID),
			observe.StringAttr("run_id", record.RunID),
			observe.StringAttr("confirmation_id", confirmationID),
			observe.StringAttr("decision", decision),
		)
		return record, nil
	}
	// CAS 未命中：依据当前状态给出明确的幂等或冲突语义。
	switch {
	case !record.ExpiresAt.After(decidedAt), record.Status == StatusExpired:
		return Confirmation{}, ErrExpired
	case record.Status == StatusRevoked:
		return Confirmation{}, ErrRevoked
	case record.Status == StatusApproved && decision == StatusApproved:
		// 重复批准：幂等成功，副作用是否重复执行由幂等记录保证。
		return record, nil
	case record.Status == StatusRejected && decision == StatusRejected:
		return record, nil
	default:
		return Confirmation{}, ErrAlreadyDecided
	}
}

// Revoke 撤销一条等待或已批准的确认（例如用户撤回授权或 Run 取消）。
// 已撤销记录重复撤销视为幂等成功；已过期/已拒绝的记录不可撤销。
func (s *Service) Revoke(ctx context.Context, appID, confirmationID string, now time.Time) (Confirmation, error) {
	if appID == "" || confirmationID == "" || now.IsZero() {
		return Confirmation{}, ErrInvalidRequest
	}
	record, transitioned, err := s.store.Revoke(ctx, appID, confirmationID, now)
	if err != nil {
		return Confirmation{}, err
	}
	if transitioned {
		observe.Info(ctx, "确认已撤销",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", appID),
			observe.StringAttr("echo_id", record.EchoID),
			observe.StringAttr("run_id", record.RunID),
			observe.StringAttr("confirmation_id", confirmationID),
		)
		return record, nil
	}
	switch {
	case !record.ExpiresAt.After(now), record.Status == StatusExpired:
		return Confirmation{}, ErrExpired
	case record.Status == StatusRevoked:
		return record, nil
	case record.Status == StatusRejected:
		return Confirmation{}, ErrAlreadyDecided
	default:
		return Confirmation{}, ErrInvalidTransition
	}
}

// Expire 显式把一条等待或已批准的确认标记为过期（允许提前强制过期，
// 例如 Run 取消时的 fail-closed 收尾）。已过期记录重复过期视为幂等成功。
func (s *Service) Expire(ctx context.Context, appID, confirmationID string, now time.Time) (Confirmation, error) {
	if appID == "" || confirmationID == "" || now.IsZero() {
		return Confirmation{}, ErrInvalidRequest
	}
	record, transitioned, err := s.store.Expire(ctx, appID, confirmationID, now)
	if err != nil {
		return Confirmation{}, err
	}
	if transitioned {
		observe.Info(ctx, "确认已过期",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", appID),
			observe.StringAttr("echo_id", record.EchoID),
			observe.StringAttr("run_id", record.RunID),
			observe.StringAttr("confirmation_id", confirmationID),
		)
		return record, nil
	}
	switch record.Status {
	case StatusExpired:
		return record, nil
	case StatusRevoked:
		return Confirmation{}, ErrRevoked
	case StatusRejected:
		return Confirmation{}, ErrAlreadyDecided
	default:
		return Confirmation{}, ErrInvalidTransition
	}
}

// ExpireDue 批量过期指定 App 下所有已到期的 waiting/approved 确认，
// 返回过期数量。适用于启动协调与定期清扫。
func (s *Service) ExpireDue(ctx context.Context, appID string, now time.Time) (int64, error) {
	if appID == "" || now.IsZero() {
		return 0, ErrInvalidRequest
	}
	affected, err := s.store.ExpireDue(ctx, appID, now)
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		observe.Info(ctx, "待确认记录已批量过期",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", appID),
			observe.Int64Attr("expired_count", affected),
		)
	}
	return affected, nil
}

// RevokeRun 撤销指定 Run 下的全部等待/已批准确认（Run 取消或终止时使用），
// 返回撤销数量。已失效记录不在撤销范围内。
func (s *Service) RevokeRun(ctx context.Context, appID, runID string, now time.Time) (int64, error) {
	if appID == "" || runID == "" || now.IsZero() {
		return 0, ErrInvalidRequest
	}
	affected, err := s.store.RevokeRun(ctx, appID, runID, now)
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		observe.Info(ctx, "Run 的待确认记录已撤销",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", appID),
			observe.StringAttr("run_id", runID),
			observe.Int64Attr("revoked_count", affected),
		)
	}
	return affected, nil
}

// Resolve 读取确认记录用于状态展示。有效期已过（含状态机已显式标记为 expired
// 以及尚未显式标记但已超期的 waiting/approved 记录）一律返回 ErrExpired，
// 便于界面呈现"已失效"；记录本身仍随错误返回。
func (s *Service) Resolve(ctx context.Context, appID, confirmationID string) (Confirmation, error) {
	record, err := s.store.Get(ctx, appID, confirmationID)
	if err != nil {
		return Confirmation{}, err
	}
	if record.Status == StatusExpired ||
		((record.Status == StatusWaiting || record.Status == StatusApproved) &&
			!record.ExpiresAt.After(s.now().UTC())) {
		return record, ErrExpired
	}
	return record, nil
}

// 编译期断言：Service 必须持续实现 runtime.ConfirmationVerifier，
// 供 Dispatcher 通过 WithConfirmationVerifier 注入后统一收敛为 ErrConfirmationRequired。
var _ runtime.ConfirmationVerifier = (*Service)(nil)

// VerifyConfirmation 实现 runtime.ConfirmationVerifier，供 Dispatcher 注入。
// 校验通过返回 nil，否则返回稳定错误（Dispatcher 统一收敛为
// ErrConfirmationRequired 公共错误）。
//
// 说明：runtime.ConfirmationRequest 目前不携带本次调用的参数摘要，
// 因此本方法只校验记录存在、授权状态、有效期与请求中的范围绑定
// （App/Echo/Run/目标/副作用/幂等键）。需要强制参数摘要匹配的调用方
// 应使用带摘要参数的 Verify；在 Dispatcher 边界补齐参数摘要传递后，
// 本方法即可统一执行摘要匹配。
func (s *Service) VerifyConfirmation(ctx context.Context, request runtime.ConfirmationRequest) error {
	return s.verify(ctx, request, "")
}

// Verify 在 VerifyConfirmation 的基础上，额外校验本次调用携带的参数摘要
// 与确认时记录的参数摘要一致；不一致返回 ErrDigestMismatch，
// 保证"参数改变后旧确认不可复用"。
func (s *Service) Verify(ctx context.Context, request runtime.ConfirmationRequest, argumentDigest string) error {
	return s.verify(ctx, request, argumentDigest)
}

func (s *Service) verify(ctx context.Context, request runtime.ConfirmationRequest, argumentDigest string) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if argumentDigest != "" && ValidateArgumentDigest(argumentDigest) != nil {
		return fmt.Errorf("%w: invalid current argument digest", ErrInvalidRequest)
	}
	record, err := s.store.Get(ctx, request.AppID, request.ConfirmationID)
	if err != nil {
		return err
	}
	err = verifyRecord(record, request, argumentDigest, s.now().UTC())
	if err != nil {
		observe.Warn(ctx, "确认验证失败",
			observe.Component("confirmation"),
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("echo_id", request.EchoID),
			observe.StringAttr("run_id", request.RunID),
			observe.StringAttr("confirmation_id", request.ConfirmationID),
			observe.StringAttr("target_type", request.TargetType),
			observe.StringAttr("target_id", request.TargetID),
			observe.StringAttr("error", err.Error()),
		)
	}
	return err
}

// verifyRecord 执行确认记录的完整匹配规则。
func verifyRecord(record Confirmation, request runtime.ConfirmationRequest, argumentDigest string, now time.Time) error {
	// 参数摘要匹配：参数改变后的旧确认不可复用。
	if argumentDigest != "" && record.ArgumentDigest != argumentDigest {
		return ErrDigestMismatch
	}
	// 范围绑定：跨 App、目标（Capability/Tool）、Echo、Run、副作用或幂等键一律拒绝。
	if record.AppID != request.AppID || record.TargetType != request.TargetType ||
		record.TargetID != request.TargetID || record.EchoID != request.EchoID ||
		record.RunID != request.RunID || record.SideEffect != request.SideEffect ||
		record.IdempotencyKey != request.IdempotencyKey {
		return ErrScopeMismatch
	}
	// 有效期：无论状态机是否显式标记，超过 expires_at 一律失效。
	if !record.ExpiresAt.After(now) {
		return ErrExpired
	}
	switch record.Status {
	case StatusApproved:
		return nil
	case StatusExpired:
		return ErrExpired
	case StatusRevoked:
		return ErrRevoked
	default: // waiting / rejected
		return ErrNotApproved
	}
}

func effectiveExpiry(expiresAt, now time.Time) (time.Time, error) {
	if expiresAt.IsZero() {
		return now.Add(DefaultLifetime), nil
	}
	if !expiresAt.After(now) {
		return time.Time{}, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidRequest)
	}
	return expiresAt.UTC(), nil
}
