package contracts

import (
	"errors"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

var (
	ErrMissingAppID     = errors.New("request context is missing app_id")
	ErrMissingEchoID    = errors.New("request context is missing echo_id")
	ErrMissingRequestID = errors.New("request context is missing request_id")
	ErrDeadlineExceeded = errors.New("request deadline has already expired")
)

// RequestContext 是所有内核治理调用必须传递的安全与可观测上下文。
// 面向公共校园服务时 UserID 可以为空，但 App、Echo 和请求标识仍不可缺失。
type RequestContext struct {
	AppID               string
	EchoID              string
	RequestID           string
	TraceID             string
	UserID              string
	SessionID           string
	RunID               string
	ParentRunID         string
	CallID              string
	CallDepth           uint16
	Deadline            time.Time
	IdempotencyKey      string
	ConfirmationID      string
	ProtocolVersion     string
	CapabilityID        string
	CallChain           []string
	CapabilityCallsUsed uint32 `json:"-"`
	CapabilityCostUsed  uint64 `json:"-"`
	// LeaseToken 仅供 Core 内部的 child Run 创建器校验，不会投影到外部协议。
	LeaseToken string `json:"-"`
	// RunCapabilityGrants 是 Run 接受时冻结的能力投影，仅供 Core 内部把
	// “已接受 Run 的授权范围不可扩张”下沉到 Dispatcher 统一治理，不会投影到
	// 外部协议；非 Run 调用（如 Web 直连）不携带。嵌套 Capability 调用经
	// NextCall 继承，保证内部调用链同样无法获得超出 Run 接受范围的权限。
	RunCapabilityGrants []capability.Grant `json:"-"`
}

func (c RequestContext) Validate(now time.Time) error {
	switch {
	case c.AppID == "":
		return ErrMissingAppID
	case c.EchoID == "":
		return ErrMissingEchoID
	case c.RequestID == "":
		return ErrMissingRequestID
	case !c.Deadline.IsZero() && !now.Before(c.Deadline):
		return ErrDeadlineExceeded
	default:
		return nil
	}
}

func (c RequestContext) NextCall() RequestContext {
	c.CallDepth++
	// Capability 调用链继承 Run 冻结的授权范围：内部调用不得超出发起 Run
	// 被接受时的投影，与 child Run 的 Grant attenuation 同一原则。
	c.RunCapabilityGrants = append([]capability.Grant(nil), c.RunCapabilityGrants...)
	return c
}
