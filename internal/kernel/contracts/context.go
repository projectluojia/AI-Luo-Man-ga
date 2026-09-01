package contracts

import (
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
)

var (
	ErrMissingAppID                = errors.New("request context is missing app_id")
	ErrMissingEchoID               = errors.New("request context is missing echo_id")
	ErrMissingRequestID            = errors.New("request context is missing request_id")
	ErrDeadlineExceeded            = errors.New("request deadline has already expired")
	ErrInvalidCapabilityProjection = errors.New("request context capability projection is invalid")
)

const (
	MaxCapabilityProjections = 64
	MaxProjectionSchemaBytes = 64 << 10
	// 为其他治理上下文、载荷和 Protobuf framing 预留空间，低于 Runtime Host 的 512 KiB 上限。
	MaxProjectionBytes = 256 << 10
)

// CapabilityProjection 是 Go 内核投影给运行时的最小 Capability 描述。
// 运行时只能据此构造调用，不会取得 Provider 的处理器或路由地址。
type CapabilityProjection struct {
	ID                  string
	Version             string
	InputSchemaJSON     string
	RequiredPermissions []string
}

// RequestContext 是所有内核治理调用必须传递的安全与可观测上下文。
// 面向公共校园服务时 UserID 可以为空，但 App、Echo 和请求标识仍不可缺失。
type RequestContext struct {
	AppID                string
	EchoID               string
	RequestID            string
	TraceID              string
	UserID               string
	SessionID            string
	RunID                string
	ParentRunID          string
	CallID               string
	CallDepth            uint16
	Deadline             time.Time
	IdempotencyKey       string
	ConfirmationID       string
	ProtocolVersion      string
	TargetType           string
	CapabilityID         string
	ServiceID            string
	ToolID               string
	PermissionScope      []string
	CallChain            []string
	ImportedCapabilities []CapabilityProjection
}

func (c RequestContext) Validate(now time.Time) error {
	switch {
	case c.AppID == "":
		return ErrMissingAppID
	case c.EchoID == "":
		return ErrMissingEchoID
	case c.RequestID == "":
		return ErrMissingRequestID
	case !validCapabilityProjections(c.ImportedCapabilities):
		return ErrInvalidCapabilityProjection
	case !c.Deadline.IsZero() && !now.Before(c.Deadline):
		return ErrDeadlineExceeded
	default:
		return nil
	}
}

func (c RequestContext) NextCall() RequestContext {
	c.CallDepth++
	return c
}

func validCapabilityProjections(values []CapabilityProjection) bool {
	if len(values) > MaxCapabilityProjections {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	projectionBytes := 0
	for _, value := range values {
		if !capability.IsStableID(value.ID) || value.InputSchemaJSON == "" ||
			len(value.InputSchemaJSON) > MaxProjectionSchemaBytes ||
			!utf8.ValidString(value.InputSchemaJSON) || !json.Valid([]byte(value.InputSchemaJSON)) {
			return false
		}
		if _, err := packagecontract.ParseVersion(value.Version); err != nil {
			return false
		}
		if _, exists := seen[value.ID]; exists {
			return false
		}
		seen[value.ID] = struct{}{}
		projectionBytes += len(value.ID) + len(value.Version) + len(value.InputSchemaJSON)
		permissions := make(map[string]struct{}, len(value.RequiredPermissions))
		for _, permission := range value.RequiredPermissions {
			if !id.IsPermission(permission) {
				return false
			}
			if _, exists := permissions[permission]; exists {
				return false
			}
			permissions[permission] = struct{}{}
			projectionBytes += len(permission)
		}
		if projectionBytes > MaxProjectionBytes {
			return false
		}
	}
	return true
}
