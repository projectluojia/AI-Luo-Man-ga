package echo_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/capability"
)

type childRunner struct {
	request kernelecho.ChildRunRequest
}

func (r *childRunner) RunChild(_ context.Context, request kernelecho.ChildRunRequest) (kernelecho.ChildRunResult, error) {
	r.request = request
	return kernelecho.ChildRunResult{RunID: "child", Status: kernelecho.RunStatusQueued}, nil
}

func (*childRunner) GetChild(context.Context, kernelecho.ChildStatusRequest) (kernelecho.ChildStatusResult, error) {
	return kernelecho.ChildStatusResult{RunID: "child", Status: kernelecho.RunStatusSucceeded}, nil
}

func TestRegisterChildCapabilitiesUsesGovernedParentIdentity(t *testing.T) {
	reg := registry.New()
	runner := &childRunner{}
	if err := kernelecho.RegisterChildCapabilities(reg, runner); err != nil {
		t.Fatal(err)
	}
	spec, handler, err := reg.ResolveCapability(kernelecho.CreateChildRunCapabilityID)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ServiceID != "run" || spec.SideEffect != capability.SideEffectExternal || spec.RequiresConfirmation {
		t.Fatalf("spec=%#v", spec)
	}
	result, err := handler(context.Background(), contracts.RequestContext{RunID: "parent", CallID: "call"},
		json.RawMessage(`{"task":"work","capability_ids":["campus.bus.routes.list"]}`))
	if err != nil || runner.request.ParentRunID != "parent" || runner.request.OriginCallID != "call" {
		t.Fatalf("request=%#v result=%s err=%v", runner.request, result, err)
	}
	var decoded kernelecho.ChildRunResult
	if err := json.Unmarshal(result, &decoded); err != nil || decoded.RunID != "child" {
		t.Fatalf("result=%s decoded=%#v err=%v", result, decoded, err)
	}
	if _, err := handler(context.Background(), contracts.RequestContext{RunID: "parent", CallID: "call"},
		json.RawMessage(`{"task":"work","unknown":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown field error=%v", err)
	}
}

func TestRegisterChildCapabilitiesRejectsMissingDependencies(t *testing.T) {
	if err := kernelecho.RegisterChildCapabilities(nil, &childRunner{}); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("nil Registry error=%v", err)
	}
	if err := kernelecho.RegisterChildCapabilities(registry.New(), nil); !errors.Is(err, registry.ErrInvalidSpec) {
		t.Fatalf("nil Runner error=%v", err)
	}
}
