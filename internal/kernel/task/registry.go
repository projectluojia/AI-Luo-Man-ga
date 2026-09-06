package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/strictschema"
)

const maxSchemaBytes = 64 << 10

// Handler 执行一次任务。处理器必须遵守传入上下文中的 deadline 与取消；
// 副作用必须使用 task.IdempotencyKey 保证可重放安全。
type Handler func(ctx context.Context, value Task) error

// TypeSpec 描述一种封闭任务类型。任务类型必须在注册表预先注册，
// 调度器绝不执行未注册类型的任务。
type TypeSpec struct {
	TypeID       string          // 稳定类型标识（如 "bus.catalog.sync"）
	ParamsSchema json.RawMessage // 严格 JSON Schema：顶层 object、拒绝未知字段
	AllowRetry   bool            // 是否允许自动重试；含不安全副作用且非幂等的类型必须为 false
	Handler      Handler         // 处理器，不可为空
}

type compiledType struct {
	spec   TypeSpec
	schema *jsonschema.Schema
}

// TypeRegistry 是封闭任务类型注册表，注册时校验类型标识与参数 Schema。
type TypeRegistry struct {
	mu    sync.RWMutex
	types map[string]*compiledType
}

// NewTypeRegistry 创建空注册表。
func NewTypeRegistry() *TypeRegistry {
	return &TypeRegistry{types: make(map[string]*compiledType)}
}

// Register 原子注册一个封闭任务类型。重复注册、非法标识、缺失处理器或
// 非法 Schema 都会失败，保证注册表只包含可安全执行的类型。
func (r *TypeRegistry) Register(spec TypeSpec) error {
	if len(spec.TypeID) == 0 || len(spec.TypeID) > maxTypeIDBytes || !id.StableLower.MatchString(spec.TypeID) {
		return fmt.Errorf("%w: 非法类型标识 %q", ErrInvalidTypeSpec, spec.TypeID)
	}
	if spec.Handler == nil {
		return fmt.Errorf("%w: %q 缺少处理器", ErrInvalidTypeSpec, spec.TypeID)
	}
	schema, err := compileParamsSchema(spec.TypeID, spec.ParamsSchema)
	if err != nil {
		return errors.Join(ErrInvalidTypeSpec, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.types[spec.TypeID]; exists {
		return ErrDuplicateType
	}
	r.types[spec.TypeID] = &compiledType{spec: spec, schema: schema}
	return nil
}

// Lookup 返回已注册类型的规格。
func (r *TypeRegistry) Lookup(typeID string) (TypeSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	compiled, ok := r.types[typeID]
	if !ok {
		return TypeSpec{}, false
	}
	return compiled.spec, true
}

// ValidateParams 校验参数是否符合已注册类型的 Schema。
// 调度器在创建与执行时都会调用，作为 Go 信任边界的双重校验。
func (r *TypeRegistry) ValidateParams(typeID string, params json.RawMessage) error {
	r.mu.RLock()
	compiled, ok := r.types[typeID]
	r.mu.RUnlock()
	if !ok {
		return ErrTaskTypeUnknown
	}
	if err := validateParamsAgainst(compiled.schema, params); err != nil {
		return err
	}
	return nil
}

func compileParamsSchema(typeID string, raw json.RawMessage) (*jsonschema.Schema, error) {
	return strictschema.CompileSchema(typeID, string(raw), "https://schema.invalid/ailuo/task/", maxSchemaBytes)
}

func validateParamsAgainst(schema *jsonschema.Schema, params json.RawMessage) error {
	if err := strictschema.ValidatePayload(schema, params, maxParamsBytes); err != nil {
		return ErrInvalidParams
	}
	return nil
}
