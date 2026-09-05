package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	ErrDuplicateID        = errors.New("registry id already exists")
	ErrInvalidSpec        = errors.New("invalid registry spec")
	ErrCapabilityNotFound = errors.New("capability is not registered")
	ErrSchemaValidation   = errors.New("payload does not satisfy registered JSON Schema")
	ErrPermissionDenied   = errors.New("required permission is not granted")
)

type Handler func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)

// CapabilityRegistration 发布一个 Provider 对外暴露的能力及其实现。
type CapabilityRegistration struct {
	Spec    capability.CapabilitySpec
	Handler Handler
}

type registeredCapability struct {
	spec    capability.CapabilitySpec
	handler Handler
	schema  *jsonschema.Schema
}

// Registry 保存不可变的 Capability 契约和实现处理器。所有能力必须在一次
// 校验完成后原子发布，避免半套包进入可调用状态。
type Registry struct {
	mu           sync.RWMutex
	capabilities map[string]registeredCapability
}

func New() *Registry {
	return &Registry{capabilities: make(map[string]registeredCapability)}
}

// Register 发布一个 Capability。
func (r *Registry) Register(registration CapabilityRegistration) error {
	return r.RegisterBatch([]CapabilityRegistration{registration})
}

// RegisterBatch 在一次发布临界区内提交全部 Capability。
func (r *Registry) RegisterBatch(registrations []CapabilityRegistration) error {
	if len(registrations) == 0 {
		return ErrInvalidSpec
	}
	prepared := make([]struct {
		registration CapabilityRegistration
		schema       *jsonschema.Schema
	}, 0, len(registrations))
	seen := make(map[string]struct{}, len(registrations))
	for _, registration := range registrations {
		if err := validateCapabilitySpec(registration.Spec, registration.Handler); err != nil {
			return err
		}
		if _, duplicate := seen[registration.Spec.ID]; duplicate {
			return fmt.Errorf("%w: capability %q", ErrDuplicateID, registration.Spec.ID)
		}
		seen[registration.Spec.ID] = struct{}{}
		compiled, err := compileInputSchema("capability."+registration.Spec.ID, registration.Spec.InputSchemaJSON)
		if err != nil {
			return fmt.Errorf("%w: capability %q input schema: %v", ErrInvalidSpec, registration.Spec.ID, err)
		}
		registration.Spec.RequiredPermissions = canonicalStrings(registration.Spec.RequiredPermissions)
		prepared = append(prepared, struct {
			registration CapabilityRegistration
			schema       *jsonschema.Schema
		}{registration: registration, schema: compiled})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range prepared {
		if _, exists := r.capabilities[item.registration.Spec.ID]; exists {
			return fmt.Errorf("%w: capability %q", ErrDuplicateID, item.registration.Spec.ID)
		}
	}
	for _, item := range prepared {
		registration := item.registration
		r.capabilities[registration.Spec.ID] = registeredCapability{
			spec: registration.Spec, handler: registration.Handler, schema: item.schema,
		}
	}
	observe.Info(context.Background(), "Capability 已原子注册到 Registry",
		observe.Component("registry"),
		observe.IntAttr("capability_count", len(prepared)),
	)
	return nil
}

func validateCapabilitySpec(spec capability.CapabilitySpec, handler Handler) error {
	switch {
	case !validStableID(spec.ID):
		return fmt.Errorf("%w: invalid capability id %q", ErrInvalidSpec, spec.ID)
	case !validVersion(spec.Version):
		return fmt.Errorf("%w: invalid capability version %q", ErrInvalidSpec, spec.Version)
	case handler == nil:
		return fmt.Errorf("%w: capability %q has no handler", ErrInvalidSpec, spec.ID)
	case !validSideEffect(spec.SideEffect):
		return fmt.Errorf("%w: capability %q has invalid side effect %q", ErrInvalidSpec, spec.ID, spec.SideEffect)
	case spec.RequiresConfirmation && spec.SideEffect != capability.SideEffectWrite && spec.SideEffect != capability.SideEffectExternal:
		return fmt.Errorf("%w: capability %q requires confirmation without a write or external side effect", ErrInvalidSpec, spec.ID)
	case !validPermissionList(spec.RequiredPermissions):
		return fmt.Errorf("%w: capability %q has invalid required permissions", ErrInvalidSpec, spec.ID)
	default:
		return nil
	}
}

func validStableID(value string) bool {
	return capability.IsStableID(value)
}

func validVersion(value string) bool {
	_, err := packagecontract.ParseVersion(value)
	return err == nil
}

func validSideEffect(value string) bool {
	switch value {
	case capability.SideEffectNone, capability.SideEffectRead, capability.SideEffectWrite, capability.SideEffectExternal:
		return true
	default:
		return false
	}
}

func validPermissionList(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !id.IsPermission(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func canonicalStrings(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}

func permissionSubset(required, granted []string) bool {
	grantedSet := make(map[string]struct{}, len(granted))
	for _, permission := range granted {
		grantedSet[permission] = struct{}{}
	}
	for _, permission := range required {
		if _, exists := grantedSet[permission]; !exists {
			return false
		}
	}
	return true
}

// NarrowPermissions 将调用者权限收窄到 Capability 声明的最小集合。
func NarrowPermissions(granted, required []string) ([]string, error) {
	if !validPermissionList(granted) || !validPermissionList(required) {
		return nil, fmt.Errorf("%w: permission scope is invalid", ErrPermissionDenied)
	}
	if !permissionSubset(required, granted) {
		return nil, ErrPermissionDenied
	}
	return canonicalStrings(required), nil
}

// ResolveCapability 解析已经完成 Schema 编译的 Capability。
func (r *Registry) ResolveCapability(id string) (capability.CapabilitySpec, Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	registered, exists := r.capabilities[id]
	if !exists {
		return capability.CapabilitySpec{}, nil, fmt.Errorf("%w: %q", ErrCapabilityNotFound, id)
	}
	return cloneCapabilitySpec(registered.spec), registered.handler, nil
}

func (r *Registry) ValidateCapabilityInput(id string, payload json.RawMessage) error {
	r.mu.RLock()
	registered, exists := r.capabilities[id]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: %q", ErrCapabilityNotFound, id)
	}
	return validatePayload(registered.schema, payload)
}

func (r *Registry) Capabilities() []capability.CapabilitySpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]capability.CapabilitySpec, 0, len(r.capabilities))
	for _, registered := range r.capabilities {
		items = append(items, cloneCapabilitySpec(registered.spec))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func cloneCapabilitySpec(spec capability.CapabilitySpec) capability.CapabilitySpec {
	spec.RequiredPermissions = append([]string(nil), spec.RequiredPermissions...)
	return spec
}
