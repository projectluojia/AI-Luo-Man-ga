package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/mod/semver"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	ErrDuplicateID        = errors.New("registry id already exists")
	ErrInvalidSpec        = errors.New("invalid registry spec")
	ErrToolNotFound       = errors.New("tool is not registered")
	ErrServiceNotFound    = errors.New("service is not registered")
	ErrCapabilityNotFound = errors.New("capability is not registered")
	ErrSchemaValidation   = errors.New("payload does not satisfy registered JSON Schema")
	ErrPermissionDenied   = errors.New("required permission is not granted")
)

const (
	SideEffectNone     = "none"
	SideEffectRead     = "read"
	SideEffectWrite    = "write"
	SideEffectExternal = "external"
)

// 治理目标类型：Dispatcher 子请求与 Runtime Host 协议共用的闭式取值。
const (
	TargetTypeCapability = "capability"
	TargetTypeTool       = "tool"
)

type Handler func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)

type ToolSpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
}

type CapabilitySpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	ServiceID            string   `json:"service_id"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
	// ToolID 声明该 Capability 直接执行的工具；Dispatcher 将其注入治理上下文的 ToolID，
	// hosted 等运行时级 Handler 据此分发。空表示 Capability 自行处理分发。
	ToolID string `json:"tool_id,omitempty"`
}

type ServiceSpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	ToolDependencies     []string `json:"tool_dependencies,omitempty"`
	RequestedPermissions []string `json:"requested_permissions,omitempty"`
}

type ToolRegistration struct {
	Spec    ToolSpec
	Handler Handler
}

type ServiceRegistration struct {
	Spec         ServiceSpec
	Capabilities map[string]struct {
		Spec    CapabilitySpec
		Handler Handler
	}
}

type registeredTool struct {
	spec    ToolSpec
	handler Handler
	schema  *jsonschema.Schema
}

type registeredCapability struct {
	spec    CapabilitySpec
	handler Handler
	schema  *jsonschema.Schema
}

// Registry 保存不可变的运行时元数据和处理器。服务及其全部 Capability 必须原子注册。
type Registry struct {
	mu           sync.RWMutex
	tools        map[string]registeredTool
	services     map[string]ServiceSpec
	capabilities map[string]registeredCapability
}

func New() *Registry {
	return &Registry{
		tools:        make(map[string]registeredTool),
		services:     make(map[string]ServiceSpec),
		capabilities: make(map[string]registeredCapability),
	}
}

func (r *Registry) RegisterTool(registration ToolRegistration) error {
	if err := validateToolSpec(registration.Spec, registration.Handler); err != nil {
		return err
	}
	compiled, err := compileInputSchema("tool."+registration.Spec.ID, registration.Spec.InputSchemaJSON)
	if err != nil {
		return fmt.Errorf("%w: tool %q input schema: %v", ErrInvalidSpec, registration.Spec.ID, err)
	}
	registration.Spec.RequiredPermissions = canonicalStrings(registration.Spec.RequiredPermissions)

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[registration.Spec.ID]; exists {
		return fmt.Errorf("%w: tool %q", ErrDuplicateID, registration.Spec.ID)
	}
	r.tools[registration.Spec.ID] = registeredTool{spec: registration.Spec, handler: registration.Handler, schema: compiled}
	observe.Debug(context.Background(), "Tool 已注册",
		observe.Component("registry"),
		observe.StringAttr("tool_id", registration.Spec.ID),
		observe.StringAttr("version", registration.Spec.Version),
		observe.StringAttr("side_effect", registration.Spec.SideEffect),
	)
	return nil
}

func (r *Registry) RegisterService(registration ServiceRegistration) error {
	if err := validateServiceSpec(registration.Spec, len(registration.Capabilities)); err != nil {
		return err
	}
	registration.Spec.ToolDependencies = canonicalStrings(registration.Spec.ToolDependencies)
	registration.Spec.RequestedPermissions = canonicalStrings(registration.Spec.RequestedPermissions)
	compiledSchemas := make(map[string]*jsonschema.Schema, len(registration.Capabilities))
	for capabilityID, capability := range registration.Capabilities {
		if err := validateCapabilitySpec(registration.Spec, capabilityID, capability.Spec, capability.Handler); err != nil {
			return err
		}
		compiled, err := compileInputSchema("capability."+capabilityID, capability.Spec.InputSchemaJSON)
		if err != nil {
			return fmt.Errorf("%w: capability %q input schema: %v", ErrInvalidSpec, capabilityID, err)
		}
		compiledSchemas[capabilityID] = compiled
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.services[registration.Spec.ID]; exists {
		return fmt.Errorf("%w: service %q", ErrDuplicateID, registration.Spec.ID)
	}
	for _, toolID := range registration.Spec.ToolDependencies {
		tool, exists := r.tools[toolID]
		if !exists {
			return fmt.Errorf("%w: dependency %q", ErrToolNotFound, toolID)
		}
		if !permissionSubset(tool.spec.RequiredPermissions, registration.Spec.RequestedPermissions) {
			return fmt.Errorf("%w: tool %q permissions exceed service %q requested permissions", ErrInvalidSpec, toolID, registration.Spec.ID)
		}
	}
	for capabilityID := range registration.Capabilities {
		if _, exists := r.capabilities[capabilityID]; exists {
			return fmt.Errorf("%w: capability %q", ErrDuplicateID, capabilityID)
		}
	}

	r.services[registration.Spec.ID] = registration.Spec
	for capabilityID, capability := range registration.Capabilities {
		spec := capability.Spec
		spec.RequiredPermissions = canonicalStrings(spec.RequiredPermissions)
		r.capabilities[capabilityID] = registeredCapability{spec: spec, handler: capability.Handler, schema: compiledSchemas[capabilityID]}
	}
	observe.Info(context.Background(), "Service 及其 Capability 已完成原子注册",
		observe.Component("registry"),
		observe.StringAttr("service_id", registration.Spec.ID),
		observe.StringAttr("version", registration.Spec.Version),
		observe.IntAttr("tool_dependency_count", len(registration.Spec.ToolDependencies)),
		observe.IntAttr("capability_count", len(registration.Capabilities)),
	)
	return nil
}

// RegisterBatch 在一次发布临界区内提交整批 Tool、Service 与 Capability。
func (r *Registry) RegisterBatch(tools []ToolRegistration, services []ServiceRegistration) error {
	if len(tools) == 0 && len(services) == 0 {
		return ErrInvalidSpec
	}
	type preparedTool struct {
		registration ToolRegistration
		schema       *jsonschema.Schema
	}
	type preparedService struct {
		registration ServiceRegistration
		schemas      map[string]*jsonschema.Schema
	}
	preparedTools := make([]preparedTool, 0, len(tools))
	toolIDs := make(map[string]struct{}, len(tools))
	batchToolSpecs := make(map[string]ToolSpec, len(tools))
	for _, registration := range tools {
		if err := validateToolSpec(registration.Spec, registration.Handler); err != nil {
			return err
		}
		if _, duplicate := toolIDs[registration.Spec.ID]; duplicate {
			return fmt.Errorf("%w: tool %q", ErrDuplicateID, registration.Spec.ID)
		}
		toolIDs[registration.Spec.ID] = struct{}{}
		compiled, err := compileInputSchema("tool."+registration.Spec.ID, registration.Spec.InputSchemaJSON)
		if err != nil {
			return fmt.Errorf("%w: tool %q input schema: %v", ErrInvalidSpec, registration.Spec.ID, err)
		}
		registration.Spec.RequiredPermissions = canonicalStrings(registration.Spec.RequiredPermissions)
		preparedTools = append(preparedTools, preparedTool{registration: registration, schema: compiled})
		batchToolSpecs[registration.Spec.ID] = registration.Spec
	}
	preparedServices := make([]preparedService, 0, len(services))
	serviceIDs := make(map[string]struct{}, len(services))
	capabilityIDs := make(map[string]struct{})
	for _, registration := range services {
		if err := validateServiceSpec(registration.Spec, len(registration.Capabilities)); err != nil {
			return err
		}
		if _, duplicate := serviceIDs[registration.Spec.ID]; duplicate {
			return fmt.Errorf("%w: service %q", ErrDuplicateID, registration.Spec.ID)
		}
		serviceIDs[registration.Spec.ID] = struct{}{}
		registration.Spec.ToolDependencies = canonicalStrings(registration.Spec.ToolDependencies)
		registration.Spec.RequestedPermissions = canonicalStrings(registration.Spec.RequestedPermissions)
		schemas := make(map[string]*jsonschema.Schema, len(registration.Capabilities))
		for capabilityID, capability := range registration.Capabilities {
			if _, duplicate := capabilityIDs[capabilityID]; duplicate {
				return fmt.Errorf("%w: capability %q", ErrDuplicateID, capabilityID)
			}
			capabilityIDs[capabilityID] = struct{}{}
			if err := validateCapabilitySpec(registration.Spec, capabilityID, capability.Spec, capability.Handler); err != nil {
				return err
			}
			compiled, err := compileInputSchema("capability."+capabilityID, capability.Spec.InputSchemaJSON)
			if err != nil {
				return fmt.Errorf("%w: capability %q input schema: %v", ErrInvalidSpec, capabilityID, err)
			}
			schemas[capabilityID] = compiled
		}
		preparedServices = append(preparedServices, preparedService{registration: registration, schemas: schemas})
	}

	r.mu.Lock()
	for _, prepared := range preparedTools {
		if _, exists := r.tools[prepared.registration.Spec.ID]; exists {
			r.mu.Unlock()
			return fmt.Errorf("%w: tool %q", ErrDuplicateID, prepared.registration.Spec.ID)
		}
	}
	for _, prepared := range preparedServices {
		registration := prepared.registration
		if _, exists := r.services[registration.Spec.ID]; exists {
			r.mu.Unlock()
			return fmt.Errorf("%w: service %q", ErrDuplicateID, registration.Spec.ID)
		}
		for capabilityID := range registration.Capabilities {
			if _, exists := r.capabilities[capabilityID]; exists {
				r.mu.Unlock()
				return fmt.Errorf("%w: capability %q", ErrDuplicateID, capabilityID)
			}
		}
		for _, toolID := range registration.Spec.ToolDependencies {
			tool, exists := r.tools[toolID]
			if !exists {
				if candidate, found := batchToolSpecs[toolID]; found {
					tool = registeredTool{spec: candidate}
					exists = true
				}
			}
			if !exists {
				r.mu.Unlock()
				return fmt.Errorf("%w: dependency %q", ErrToolNotFound, toolID)
			}
			if !permissionSubset(tool.spec.RequiredPermissions, registration.Spec.RequestedPermissions) {
				r.mu.Unlock()
				return fmt.Errorf("%w: tool %q permissions exceed service %q requested permissions", ErrInvalidSpec, toolID, registration.Spec.ID)
			}
		}
	}
	for _, prepared := range preparedTools {
		registration := prepared.registration
		r.tools[registration.Spec.ID] = registeredTool{
			spec: registration.Spec, handler: registration.Handler, schema: prepared.schema,
		}
	}
	for _, prepared := range preparedServices {
		registration := prepared.registration
		r.services[registration.Spec.ID] = registration.Spec
		for capabilityID, capability := range registration.Capabilities {
			spec := capability.Spec
			spec.RequiredPermissions = canonicalStrings(spec.RequiredPermissions)
			r.capabilities[capabilityID] = registeredCapability{
				spec: spec, handler: capability.Handler, schema: prepared.schemas[capabilityID],
			}
		}
	}
	r.mu.Unlock()
	observe.Info(context.Background(), "安装目录已原子注册到 Registry",
		observe.Component("registry"),
		observe.IntAttr("tool_count", len(preparedTools)),
		observe.IntAttr("service_count", len(preparedServices)),
		observe.IntAttr("capability_count", len(capabilityIDs)),
	)
	return nil
}

var (
	stableIDPattern   = id.StableLower
	permissionPattern = id.Permission
)

func validateToolSpec(spec ToolSpec, handler Handler) error {
	switch {
	case !validStableID(spec.ID):
		return fmt.Errorf("%w: invalid tool id %q", ErrInvalidSpec, spec.ID)
	case !validVersion(spec.Version):
		return fmt.Errorf("%w: invalid tool version %q", ErrInvalidSpec, spec.Version)
	case handler == nil:
		return fmt.Errorf("%w: tool %q has no handler", ErrInvalidSpec, spec.ID)
	case !validSideEffect(spec.SideEffect):
		return fmt.Errorf("%w: tool %q has invalid side effect %q", ErrInvalidSpec, spec.ID, spec.SideEffect)
	case spec.RequiresConfirmation && spec.SideEffect != SideEffectWrite && spec.SideEffect != SideEffectExternal:
		return fmt.Errorf("%w: tool %q requires confirmation without a write or external side effect", ErrInvalidSpec, spec.ID)
	case !validPermissionList(spec.RequiredPermissions):
		return fmt.Errorf("%w: tool %q has invalid required permissions", ErrInvalidSpec, spec.ID)
	default:
		return nil
	}
}

func validateServiceSpec(spec ServiceSpec, capabilityCount int) error {
	switch {
	case !validStableID(spec.ID):
		return fmt.Errorf("%w: invalid service id %q", ErrInvalidSpec, spec.ID)
	case !validVersion(spec.Version):
		return fmt.Errorf("%w: invalid service version %q", ErrInvalidSpec, spec.Version)
	case capabilityCount == 0:
		return fmt.Errorf("%w: service %q has no capabilities", ErrInvalidSpec, spec.ID)
	case !validIDList(spec.ToolDependencies):
		return fmt.Errorf("%w: service %q has invalid tool dependencies", ErrInvalidSpec, spec.ID)
	case !validPermissionList(spec.RequestedPermissions):
		return fmt.Errorf("%w: service %q has invalid requested permissions", ErrInvalidSpec, spec.ID)
	default:
		return nil
	}
}

func validateCapabilitySpec(service ServiceSpec, mapID string, spec CapabilitySpec, handler Handler) error {
	switch {
	case mapID != spec.ID:
		return fmt.Errorf("%w: capability map id %q does not match spec id %q", ErrInvalidSpec, mapID, spec.ID)
	case !validStableID(spec.ID):
		return fmt.Errorf("%w: invalid capability id %q", ErrInvalidSpec, spec.ID)
	case !validVersion(spec.Version):
		return fmt.Errorf("%w: invalid capability version %q", ErrInvalidSpec, spec.Version)
	case spec.ServiceID != service.ID:
		return fmt.Errorf("%w: capability %q belongs to service %q, not %q", ErrInvalidSpec, spec.ID, spec.ServiceID, service.ID)
	case handler == nil:
		return fmt.Errorf("%w: capability %q has no handler", ErrInvalidSpec, spec.ID)
	case !validSideEffect(spec.SideEffect):
		return fmt.Errorf("%w: capability %q has invalid side effect %q", ErrInvalidSpec, spec.ID, spec.SideEffect)
	case spec.RequiresConfirmation && spec.SideEffect != SideEffectWrite && spec.SideEffect != SideEffectExternal:
		return fmt.Errorf("%w: capability %q requires confirmation without a write or external side effect", ErrInvalidSpec, spec.ID)
	case !validPermissionList(spec.RequiredPermissions):
		return fmt.Errorf("%w: capability %q has invalid required permissions", ErrInvalidSpec, spec.ID)
	case !permissionSubset(spec.RequiredPermissions, service.RequestedPermissions):
		return fmt.Errorf("%w: capability %q permissions exceed service %q requested permissions", ErrInvalidSpec, spec.ID, service.ID)
	case spec.ToolID != "" && !validStableID(spec.ToolID):
		return fmt.Errorf("%w: capability %q has invalid tool id %q", ErrInvalidSpec, spec.ID, spec.ToolID)
	default:
		return nil
	}
}

func validStableID(value string) bool {
	return stableIDPattern.MatchString(value)
}

func validVersion(value string) bool {
	return semver.IsValid("v" + value)
}

func validSideEffect(value string) bool {
	switch value {
	case SideEffectNone, SideEffectRead, SideEffectWrite, SideEffectExternal:
		return true
	default:
		return false
	}
}

func validIDList(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validStableID(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validPermissionList(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !permissionPattern.MatchString(value) {
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

func NarrowPermissions(granted, required []string) ([]string, error) {
	if !validPermissionList(granted) || !validPermissionList(required) {
		return nil, fmt.Errorf("%w: permission scope is invalid", ErrPermissionDenied)
	}
	if !permissionSubset(required, granted) {
		return nil, ErrPermissionDenied
	}
	return canonicalStrings(required), nil
}

func (r *Registry) ResolveCapability(id string) (CapabilitySpec, Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	capability, exists := r.capabilities[id]
	if !exists {
		return CapabilitySpec{}, nil, fmt.Errorf("%w: %q", ErrCapabilityNotFound, id)
	}
	return cloneCapabilitySpec(capability.spec), capability.handler, nil
}

func (r *Registry) ResolveTool(serviceID, toolID string) (ToolSpec, Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	service, exists := r.services[serviceID]
	if !exists {
		return ToolSpec{}, nil, fmt.Errorf("%w: %q", ErrServiceNotFound, serviceID)
	}
	if !slices.Contains(service.ToolDependencies, toolID) {
		return ToolSpec{}, nil, fmt.Errorf("%w: service %q does not declare tool %q", ErrToolNotFound, serviceID, toolID)
	}
	tool, exists := r.tools[toolID]
	if !exists {
		return ToolSpec{}, nil, fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	return cloneToolSpec(tool.spec), tool.handler, nil
}

func (r *Registry) ValidateCapabilityInput(id string, payload json.RawMessage) error {
	r.mu.RLock()
	capability, exists := r.capabilities[id]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: %q", ErrCapabilityNotFound, id)
	}
	return validatePayload(capability.schema, payload)
}

func (r *Registry) ValidateToolInput(serviceID, toolID string, payload json.RawMessage) error {
	r.mu.RLock()
	service, serviceExists := r.services[serviceID]
	tool, toolExists := r.tools[toolID]
	r.mu.RUnlock()
	if !serviceExists {
		return fmt.Errorf("%w: %q", ErrServiceNotFound, serviceID)
	}
	if !slices.Contains(service.ToolDependencies, toolID) || !toolExists {
		return fmt.Errorf("%w: service %q does not declare tool %q", ErrToolNotFound, serviceID, toolID)
	}
	return validatePayload(tool.schema, payload)
}

func (r *Registry) Services() []ServiceSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]ServiceSpec, 0, len(r.services))
	for _, spec := range r.services {
		items = append(items, cloneServiceSpec(spec))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (r *Registry) Tools() []ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		items = append(items, cloneToolSpec(tool.spec))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (r *Registry) Capabilities() []CapabilitySpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]CapabilitySpec, 0, len(r.capabilities))
	for _, capability := range r.capabilities {
		items = append(items, cloneCapabilitySpec(capability.spec))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func cloneServiceSpec(spec ServiceSpec) ServiceSpec {
	spec.ToolDependencies = append([]string(nil), spec.ToolDependencies...)
	spec.RequestedPermissions = append([]string(nil), spec.RequestedPermissions...)
	return spec
}

func cloneToolSpec(spec ToolSpec) ToolSpec {
	spec.RequiredPermissions = append([]string(nil), spec.RequiredPermissions...)
	return spec
}

func cloneCapabilitySpec(spec CapabilitySpec) CapabilitySpec {
	spec.RequiredPermissions = append([]string(nil), spec.RequiredPermissions...)
	return spec
}
