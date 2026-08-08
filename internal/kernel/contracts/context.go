package contracts

import (
	"errors"
	"time"
)

var (
	ErrMissingAppID     = errors.New("request context is missing app_id")
	ErrMissingEchoID    = errors.New("request context is missing echo_id")
	ErrMissingRequestID = errors.New("request context is missing request_id")
	ErrDeadlineExceeded = errors.New("request deadline has already expired")
	ErrDataUnavailable  = errors.New("authoritative data is unavailable")
	ErrDataIncomplete   = errors.New("data freshness metadata is incomplete")
	ErrDataUntrusted    = errors.New("data source is not authoritative")
	ErrDataExpired      = errors.New("authoritative data has expired")
)

// RequestContext 是所有内核治理调用必须传递的安全与可观测上下文。
// 面向公共校园服务时 UserID 可以为空，但 App、Echo 和请求标识仍不可缺失。
type RequestContext struct {
	AppID           string
	EchoID          string
	RequestID       string
	TraceID         string
	UserID          string
	SessionID       string
	RunID           string
	ParentRunID     string
	CallID          string
	CallDepth       uint16
	Deadline        time.Time
	IdempotencyKey  string
	ConfirmationID  string
	ProtocolVersion string
	TargetType      string
	CapabilityID    string
	ServiceID       string
	ToolID          string
	PermissionScope []string
	CallChain       []string
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

func (c RequestContext) Child() RequestContext {
	c.CallDepth++
	return c
}
