package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/subagent"
)

type captureRunner struct {
	request echo.ChildRunRequest
	err     error
}

func (r *captureRunner) RunChild(_ context.Context, request echo.ChildRunRequest) (echo.ChildRunResult, error) {
	r.request = request
	if r.err != nil {
		return echo.ChildRunResult{}, r.err
	}
	return echo.ChildRunResult{RunID: "child", Result: "done"}, nil
}

func TestRegisterRoutesGovernedParentIdentityAndStrictInput(t *testing.T) {
	reg := registry.New()
	runner := &captureRunner{}
	if err := subagent.Register(reg, runner); err != nil {
		t.Fatal(err)
	}
	spec, handler, err := reg.ResolveCapability(subagent.CapabilityID)
	if err != nil {
		t.Fatal(err)
	}
	if spec.SideEffect != registry.SideEffectExternal || spec.RequiresConfirmation {
		t.Fatalf("spec=%#v", spec)
	}
	result, err := handler(context.Background(), contracts.RequestContext{
		RunID: "parent", CallID: "call",
	}, json.RawMessage(`{"task":"work","capability_ids":["campus.bus.routes.list"]}`))
	if err != nil || runner.request.ParentRunID != "parent" || runner.request.OriginCallID != "call" ||
		len(runner.request.CapabilityScope) != 1 {
		t.Fatalf("request=%#v result=%s err=%v", runner.request, result, err)
	}
	var decoded echo.ChildRunResult
	if err := json.Unmarshal(result, &decoded); err != nil || decoded.RunID != "child" || decoded.Result != "done" {
		t.Fatalf("result=%s decoded=%#v err=%v", result, decoded, err)
	}
	if _, err := handler(context.Background(), contracts.RequestContext{
		RunID: "parent", CallID: "call",
	}, json.RawMessage(`{"task":"work","unknown":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown field error=%v", err)
	}
}

func TestRegisterRejectsMissingDependencies(t *testing.T) {
	if err := subagent.Register(nil, &captureRunner{}); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("nil Registry error=%v", err)
	}
	if err := subagent.Register(registry.New(), nil); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("nil Runner error=%v", err)
	}
}
