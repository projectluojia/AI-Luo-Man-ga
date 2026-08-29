package confirmation

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

func TestDigestCanonicalizesArguments(t *testing.T) {
	t.Parallel()
	first, err := Digest([]byte(`{"amount":10,"note":"乘车","target":"武昌"}`))
	if err != nil {
		t.Fatalf("digest first form: %v", err)
	}
	// 键顺序不同但内容相同的 JSON 必须产生相同的摘要。
	second, err := Digest([]byte(`{"target":"武昌","amount":10,"note":"乘车"}`))
	if err != nil {
		t.Fatalf("digest reordered form: %v", err)
	}
	if first != second {
		t.Fatalf("canonical digest differs: %q != %q", first, second)
	}
	if err := ValidateArgumentDigest(first); err != nil {
		t.Fatalf("digest format invalid: %v", err)
	}
	// 参数改变后摘要必须改变。
	changed, err := Digest([]byte(`{"amount":100,"note":"乘车","target":"武昌"}`))
	if err != nil {
		t.Fatalf("digest changed form: %v", err)
	}
	if changed == first {
		t.Fatal("changed arguments must produce a different digest")
	}
}

func TestDigestTreatsEmptyArgumentsAsEmptyObject(t *testing.T) {
	t.Parallel()
	empty, err := Digest(nil)
	if err != nil {
		t.Fatalf("digest nil: %v", err)
	}
	object, err := Digest([]byte(`{}`))
	if err != nil {
		t.Fatalf("digest {}: %v", err)
	}
	if empty != object {
		t.Fatalf("nil and {} must produce the same digest: %q != %q", empty, object)
	}
}

func TestDigestRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := Digest([]byte(`{"broken":`)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid JSON got %v, want ErrInvalidRequest", err)
	}
	oversized := make([]byte, maxArgumentBytes+1)
	for index := range oversized {
		oversized[index] = 'a'
	}
	if _, err := Digest(oversized); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized arguments got %v, want ErrInvalidRequest", err)
	}
}

func TestValidateStatusAndTargets(t *testing.T) {
	t.Parallel()
	for _, status := range []string{StatusWaiting, StatusApproved, StatusRejected, StatusExpired, StatusRevoked} {
		if err := ValidateStatus(status); err != nil {
			t.Fatalf("status %q should be valid: %v", status, err)
		}
	}
	if err := ValidateStatus("executed"); !errors.Is(err, ErrInvalidRequest) {
		// 执行结果不属于确认状态机：状态机不允许把执行结果伪报为状态。
		t.Fatalf("unknown status got %v, want ErrInvalidRequest", err)
	}
	for _, target := range []string{TargetTypeCapability, TargetTypeTool} {
		if err := ValidateTargetType(target); err != nil {
			t.Fatalf("target type %q should be valid: %v", target, err)
		}
	}
	if err := ValidateTargetType("database"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid target type got %v, want ErrInvalidRequest", err)
	}
	for _, sideEffect := range []string{SideEffectWrite, SideEffectExternal} {
		if err := ValidateSideEffect(sideEffect); err != nil {
			t.Fatalf("side effect %q should be valid: %v", sideEffect, err)
		}
	}
	if err := ValidateSideEffect("read"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("read side effect got %v, want ErrInvalidRequest", err)
	}
}

func TestValidateRequestRejectsPartialBindings(t *testing.T) {
	t.Parallel()
	valid := runtime.ConfirmationRequest{
		AppID: "app", EchoID: "echo", RunID: "run", ConfirmationID: "confirmation-1",
		TargetType: TargetTypeCapability, TargetID: "campus.bus.notify",
		SideEffect: SideEffectExternal, IdempotencyKey: "operation-1",
		ArgumentDigest: strings.Repeat("a", 64),
	}
	if err := ValidateRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for name, mutate := range map[string]func(*runtime.ConfirmationRequest){
		"缺 App":   func(r *runtime.ConfirmationRequest) { r.AppID = "" },
		"缺 Echo":  func(r *runtime.ConfirmationRequest) { r.EchoID = "" },
		"缺 Run":   func(r *runtime.ConfirmationRequest) { r.RunID = "" },
		"缺确认标识":   func(r *runtime.ConfirmationRequest) { r.ConfirmationID = "" },
		"非法目标类型":  func(r *runtime.ConfirmationRequest) { r.TargetType = "database" },
		"缺目标":     func(r *runtime.ConfirmationRequest) { r.TargetID = "" },
		"非法副作用类型": func(r *runtime.ConfirmationRequest) { r.SideEffect = "read" },
		"非法幂等键":   func(r *runtime.ConfirmationRequest) { r.IdempotencyKey = "bad key!" },
		"缺参数摘要":   func(r *runtime.ConfirmationRequest) { r.ArgumentDigest = "" },
		"非法参数摘要":  func(r *runtime.ConfirmationRequest) { r.ArgumentDigest = "zz" },
		"超长确认标识":  func(r *runtime.ConfirmationRequest) { r.ConfirmationID = strings.Repeat("x", 257) },
	} {
		request := valid
		mutate(&request)
		if err := ValidateRequest(request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s got %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestValidateConfirmationRequiresCompleteBinding(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	valid := Confirmation{
		AppID: "app", ConfirmationID: "confirmation-1",
		EchoID: "echo", RunID: "run", CallID: "call-1",
		CapabilityID: "campus.bus.notify", TargetType: TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: SideEffectExternal,
		IdempotencyKey: "operation-1",
		ArgumentDigest: strings.Repeat("ab", 32),
		Status:         StatusWaiting,
		ExpiresAt:      now.Add(time.Hour),
		CreatedAt:      now,
	}
	if err := ValidateConfirmation(valid); err != nil {
		t.Fatalf("valid confirmation rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Confirmation){
		"缺 App":  func(c *Confirmation) { c.AppID = "" },
		"缺确认标识":  func(c *Confirmation) { c.ConfirmationID = "" },
		"缺 Echo": func(c *Confirmation) { c.EchoID = "" },
		"缺 Run":  func(c *Confirmation) { c.RunID = "" },
		"缺 Call": func(c *Confirmation) { c.CallID = "" },
		"非法摘要":   func(c *Confirmation) { c.ArgumentDigest = "short" },
		"非法状态":   func(c *Confirmation) { c.Status = "executed" },
		"过期早于创建": func(c *Confirmation) { c.ExpiresAt = now.Add(-time.Minute) },
		"waiting 带决策时间": func(c *Confirmation) {
			decided := now
			c.DecidedAt = &decided
		},
		"approved 缺决策时间": func(c *Confirmation) {
			c.Status = StatusApproved
			c.ConfirmedBy = "user-1"
		},
		"approved 缺决策人": func(c *Confirmation) {
			decided := now
			c.Status = StatusApproved
			c.DecidedAt = &decided
		},
		"非法目标类型":  func(c *Confirmation) { c.TargetType = "database" },
		"非法副作用类型": func(c *Confirmation) { c.SideEffect = "read" },
		"非法幂等键":   func(c *Confirmation) { c.IdempotencyKey = "bad key!" },
	} {
		record := valid
		mutate(&record)
		if err := ValidateConfirmation(record); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s got %v, want ErrInvalidRequest", name, err)
		}
	}
	// 已决策状态的一致性约束：expired/revoked 必须携带决策时间。
	decided := now.Add(time.Minute)
	expired := valid
	expired.Status = StatusExpired
	expired.DecidedAt = &decided
	if err := ValidateConfirmation(expired); err != nil {
		t.Fatalf("expired with decision time rejected: %v", err)
	}
}
