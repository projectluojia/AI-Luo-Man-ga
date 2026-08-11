package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 测试用参数 Schema：顶层 object、拒绝未知字段、batch 必须为正整数。
const testParamsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "batch": {"type": "integer", "minimum": 1}
  },
  "required": ["batch"]
}`

// newTestTask 构造一个通过 ValidateNewTask 的新任务（基于真实当前时间）。
func newTestTask() Task {
	return newTestTaskAt(time.Now().UTC())
}

// newTestTaskAt 以指定时间构造新任务，便于测试与可控时钟保持同一时间基准。
func newTestTaskAt(now time.Time) Task {
	return Task{
		AppID:          "campus-services",
		TaskID:         "task-1",
		Type:           "bus.catalog.sync",
		Status:         StatusQueued,
		Attempt:        1,
		MaxAttempts:    3,
		Deadline:       now.Add(time.Hour),
		AvailableAt:    now,
		IdempotencyKey: "key-task-1",
		Params:         []byte(`{"batch":10}`),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func TestValidateNewTaskAcceptsValidTask(t *testing.T) {
	if err := ValidateNewTask(newTestTask()); err != nil {
		t.Fatalf("合法任务被拒绝：%v", err)
	}
}

func TestValidateNewTaskRejectsInvalidFields(t *testing.T) {
	base := newTestTask()
	cases := map[string]func(*Task){
		"空 app_id":                  func(value *Task) { value.AppID = "" },
		"非法 app_id":                 func(value *Task) { value.AppID = "非法字符" },
		"空 task_id":                 func(value *Task) { value.TaskID = "" },
		"非法 task_id":                func(value *Task) { value.TaskID = "-bad" },
		"超长 task_id":                func(value *Task) { value.TaskID = strings.Repeat("a", 129) },
		"空任务类型":                     func(value *Task) { value.Type = "" },
		"非 queued 状态":               func(value *Task) { value.Status = StatusRunning },
		"attempt 非 1":               func(value *Task) { value.Attempt = 2 },
		"max_attempts 为 0":          func(value *Task) { value.MaxAttempts = 0 },
		"max_attempts 超限":           func(value *Task) { value.MaxAttempts = 33 },
		"created_at 为零":             func(value *Task) { value.CreatedAt = time.Time{} },
		"updated_at 为零":             func(value *Task) { value.UpdatedAt = time.Time{} },
		"deadline 不晚于创建时间":          func(value *Task) { value.Deadline = value.CreatedAt },
		"available_at 早于创建时间":       func(value *Task) { value.AvailableAt = value.CreatedAt.Add(-time.Second) },
		"available_at 不早于 deadline": func(value *Task) { value.AvailableAt = value.Deadline },
		"新任务携带租约令牌":                 func(value *Task) { value.LeaseToken = "lease" },
		"新任务携带租约到期": func(value *Task) {
			expires := value.CreatedAt.Add(time.Minute)
			value.LeaseExpiresAt = &expires
		},
		"非法幂等键":    func(value *Task) { value.IdempotencyKey = "非法键" },
		"空幂等键":     func(value *Task) { value.IdempotencyKey = "" },
		"参数非 JSON": func(value *Task) { value.Params = []byte("not json") },
		// 注意：ValidateNewTask 只要求合法 JSON；参数必须是 object 且拒绝未知字段
		// 的严格约束由 TypeRegistry 的 Schema 负责（TestTypeRegistryValidatesParams 覆盖）。
		"参数携带错误分类": func(value *Task) { value.ErrorClass = ErrorClassNonRetryable },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if err := ValidateNewTask(value); !errors.Is(err, ErrInvalidTask) {
				t.Fatalf("期望 ErrInvalidTask，实际 err=%v", err)
			}
		})
	}
}

func TestValidateNewTaskRejectsOversizedParams(t *testing.T) {
	value := newTestTask()
	value.Params = []byte(`{"batch":` + strings.Repeat("1", maxParamsBytes+1) + `}`)
	if err := ValidateNewTask(value); !errors.Is(err, ErrInvalidTask) {
		t.Fatalf("期望 ErrInvalidTask，实际 err=%v", err)
	}
}

func TestClassifyFailureMapsToStableErrorClasses(t *testing.T) {
	cases := map[string]struct {
		input ErrorClass
		want  ErrorClass
	}{
		"无错误":          {input: ClassifyFailure(nil), want: ErrorClassNone},
		"超时":           {input: ClassifyFailure(context.DeadlineExceeded), want: ErrorClassDeadline},
		"显式可重试":        {input: ClassifyFailure(Retryable(errors.New("transient"))), want: ErrorClassRetryable},
		"包装的可重试":       {input: ClassifyFailure(errors.Join(errors.New("cause"), Retryable(errors.New("transient")))), want: ErrorClassRetryable},
		"普通错误失败关闭":     {input: ClassifyFailure(errors.New("boom")), want: ErrorClassNonRetryable},
		"包装错误失败关闭":     {input: ClassifyFailure(fmtWrap(errors.New("boom"))), want: ErrorClassNonRetryable},
		"可重试包装超时按超时处理": {input: ClassifyFailure(Retryable(context.DeadlineExceeded)), want: ErrorClassDeadline},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			if item.input != item.want {
				t.Fatalf("分类=%q，期望 %q", item.input, item.want)
			}
		})
	}
	if Retryable(nil) != nil {
		t.Fatal("Retryable(nil) 必须返回 nil")
	}
}

type wrapError struct{ cause error }

func (e *wrapError) Error() string { return "wrap: " + e.cause.Error() }
func (e *wrapError) Unwrap() error { return e.cause }

func fmtWrap(err error) error { return &wrapError{cause: err} }

func TestTypeRegistryRegistersClosedSet(t *testing.T) {
	registry := NewTypeRegistry()
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { return nil },
	}); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	if registry.Len() != 1 {
		t.Fatalf("注册数量=%d，期望 1", registry.Len())
	}
	spec, ok := registry.Lookup("bus.catalog.sync")
	if !ok || !spec.AllowRetry {
		t.Fatalf("Lookup 失败：%#v", spec)
	}
	if _, ok := registry.Lookup("unknown.type"); ok {
		t.Fatal("未注册类型不应被查找到")
	}
}

func TestTypeRegistryRejectsInvalidSpecs(t *testing.T) {
	validHandler := func(context.Context, Task) error { return nil }
	validSchema := json.RawMessage(testParamsSchema)
	cases := map[string]TypeSpec{
		"非法类型标识": {
			TypeID: "Bad ID", ParamsSchema: validSchema, AllowRetry: true, Handler: validHandler,
		},
		"数字开头类型标识": {
			TypeID: "1bus", ParamsSchema: validSchema, AllowRetry: true, Handler: validHandler,
		},
		"缺少处理器": {
			TypeID: "bus.catalog.sync", ParamsSchema: validSchema, AllowRetry: true,
		},
		"空 Schema": {
			TypeID: "bus.catalog.sync", ParamsSchema: nil, AllowRetry: true, Handler: validHandler,
		},
		"顶层非 object Schema": {
			TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(`{"type":"array"}`), AllowRetry: true, Handler: validHandler,
		},
		"未拒绝未知字段的 Schema": {
			TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(`{"type":"object"}`), AllowRetry: true, Handler: validHandler,
		},
		"外部引用的 Schema": {
			TypeID:       "bus.catalog.sync",
			ParamsSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"a":{"$ref":"https://evil.example/schema"}}}`),
			AllowRetry:   true, Handler: validHandler,
		},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			registry := NewTypeRegistry()
			if err := registry.Register(spec); !errors.Is(err, ErrInvalidTypeSpec) {
				t.Fatalf("期望 ErrInvalidTypeSpec，实际 err=%v", err)
			}
		})
	}
}

func TestTypeRegistryRejectsDuplicateRegistration(t *testing.T) {
	registry := NewTypeRegistry()
	spec := TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { return nil },
	}
	if err := registry.Register(spec); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(spec); !errors.Is(err, ErrDuplicateType) {
		t.Fatalf("期望 ErrDuplicateType，实际 err=%v", err)
	}
}

func TestTypeRegistryValidatesParams(t *testing.T) {
	registry := NewTypeRegistry()
	if err := registry.Register(TypeSpec{
		TypeID: "bus.catalog.sync", ParamsSchema: json.RawMessage(testParamsSchema),
		AllowRetry: true, Handler: func(context.Context, Task) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateParams("bus.catalog.sync", json.RawMessage(`{"batch":10}`)); err != nil {
		t.Fatalf("合法参数被拒绝：%v", err)
	}
	cases := map[string]struct {
		typeID string
		params json.RawMessage
	}{
		"未注册类型":    {"not.registered", json.RawMessage(`{"batch":1}`)},
		"缺失必填字段":   {"bus.catalog.sync", json.RawMessage(`{}`)},
		"未知字段":     {"bus.catalog.sync", json.RawMessage(`{"batch":1,"extra":true}`)},
		"违反最小值":    {"bus.catalog.sync", json.RawMessage(`{"batch":0}`)},
		"类型不符":     {"bus.catalog.sync", json.RawMessage(`{"batch":"10"}`)},
		"非 object": {"bus.catalog.sync", json.RawMessage(`[1,2]`)},
		"重复键":      {"bus.catalog.sync", json.RawMessage(`{"batch":1,"batch":2}`)},
		"空参数":      {"bus.catalog.sync", json.RawMessage(``)},
		"超长参数":     {"bus.catalog.sync", json.RawMessage(`{"batch":` + strings.Repeat("1", maxParamsBytes) + `}`)},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			err := registry.ValidateParams(item.typeID, item.params)
			if item.typeID != "bus.catalog.sync" {
				if !errors.Is(err, ErrTaskTypeUnknown) {
					t.Fatalf("期望 ErrTaskTypeUnknown，实际 err=%v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("期望 ErrInvalidParams，实际 err=%v", err)
			}
		})
	}
}
