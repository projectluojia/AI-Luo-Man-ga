// Package authorization 提供 Core 使用的结构化 Capability 授权判断。
package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

var (
	ErrDenied                  = errors.New("capability authorization denied")
	ErrInvalidResource         = errors.New("capability resource binding is invalid")
	ErrPrincipalRequired       = errors.New("capability requires an authenticated principal")
	ErrRelationshipUnavailable = errors.New("capability relationship data is unavailable")
	ErrBudgetExceeded          = errors.New("capability grant budget exceeded")
)

// Request 是一次 Capability 授权判断的 PARC 请求。
type Request struct {
	AppID        string
	Principal    string
	RunID        string
	CapabilityID string
	Payload      []byte
	Now          time.Time
	CallsUsed    uint32
	CostUsed     uint64
}

// Decision 是授权结果和 Dispatcher 必须执行的安全义务。
type Decision struct {
	Grant               capability.Grant
	RequireIdempotency  bool
	RequireConfirmation bool
}

// RelationshipChecker 查询主体与资源之间的受治理关系。
type RelationshipChecker interface {
	Check(context.Context, string, string, string, string) (bool, error)
}

// Authorize 在每次调用时检查主体、Capability、资源、时间和预算。
func Authorize(ctx context.Context, spec capability.CapabilitySpec, request Request, grants []capability.Grant, relationships RelationshipChecker) (Decision, error) {
	if err := capability.ValidateAuthorizationSpec(spec.Authorization); err != nil {
		return Decision{}, errors.Join(ErrDenied, err)
	}
	if spec.Authorization.Principal == capability.PrincipalCurrentUser && request.Principal == "public" {
		return Decision{}, errors.Join(ErrDenied, ErrPrincipalRequired)
	}
	resourceID, err := resourceID(spec.Authorization.ResourceIDFrom, request.Payload)
	if err != nil {
		return Decision{}, errors.Join(ErrDenied, err)
	}
	if err := capability.ValidateExecutionSpec(spec.Execution); err != nil {
		return Decision{}, errors.Join(ErrDenied, err)
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	for _, candidate := range grants {
		grant, normalizeErr := capability.NormalizeGrant(candidate)
		if normalizeErr != nil || grant.AppID != request.AppID || grant.CapabilityID != request.CapabilityID ||
			grant.Principal != request.Principal && grant.Principal != capability.PrincipalAny ||
			!grant.NotBefore.IsZero() && request.Now.Before(grant.NotBefore) || request.Now.After(grant.ExpiresAt) ||
			grant.Audience != "" && grant.Audience != request.RunID ||
			grant.Resource.Type != spec.Authorization.ResourceType && grant.Resource.Type != "any" {
			continue
		}
		if len(grant.Resource.IDs) > 0 && !contains(grant.Resource.IDs, resourceID) {
			continue
		}
		if grant.Resource.Relation != "" {
			if relationships == nil {
				return Decision{}, errors.Join(ErrDenied, ErrRelationshipUnavailable)
			}
			if resourceID == "" {
				continue
			}
			allowed, checkErr := relationships.Check(ctx, request.Principal, grant.Resource.Relation, grant.Resource.Type, resourceID)
			if checkErr != nil {
				return Decision{}, errors.Join(ErrDenied, ErrRelationshipUnavailable)
			}
			if !allowed {
				continue
			}
		}
		if request.CallsUsed >= grant.MaxCalls || grant.MaxCostMicrousd != 0 && request.CostUsed >= grant.MaxCostMicrousd {
			return Decision{}, errors.Join(ErrDenied, ErrBudgetExceeded)
		}
		return Decision{
			Grant:               grant,
			RequireIdempotency:  spec.Execution.Replay == capability.ReplayIdempotencyKey || spec.Execution.Replay == capability.ReplayNever,
			RequireConfirmation: spec.Execution.ConfirmationFloor == capability.ConfirmationRequired,
		}, nil
	}
	return Decision{}, ErrDenied
}

func resourceID(pointer string, payload []byte) (string, error) {
	if pointer == "" {
		return "", nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return "", errors.Join(ErrInvalidResource, err)
	}
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		object, ok := value.(map[string]any)
		if !ok {
			return "", ErrInvalidResource
		}
		value, ok = object[part]
		if !ok {
			return "", fmt.Errorf("%w: missing %q", ErrInvalidResource, part)
		}
	}
	result, ok := value.(string)
	if !ok || result == "" || !capability.IsStableID(result) {
		return "", ErrInvalidResource
	}
	return result, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
