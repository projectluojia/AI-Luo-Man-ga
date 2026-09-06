package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/childrun"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

type childPolicySource struct{ config appconfig.Config }

func (s childPolicySource) Current(context.Context, string) (appconfig.Config, error) {
	return s.config, nil
}

func (s childPolicySource) Revision(context.Context, string, string) (appconfig.Config, error) {
	return s.config, nil
}

func childGrant(appID, id, capabilityID string) capability.Grant {
	return capability.Grant{
		ID: id, AppID: appID, Principal: capability.PrincipalAny, CapabilityID: capabilityID,
		Resource:  capability.ResourceScope{Type: "campus.bus.route"},
		ExpiresAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), MaxCalls: 10,
		PolicyRevision: "policy",
	}
}

// createChildPayload 组装 run.create_child 的请求体；cost/timeout 缺省走
// 父 Run 的合法区间，便于单字段回归。
func createChildPayload(grants []capability.Grant, mutate func(map[string]any)) json.RawMessage {
	body := map[string]any{
		"task": "child task", "capability_grants": grants,
		"max_steps": 2, "max_capability_calls": 2, "max_execution_units": 100,
		"max_output_bytes": 1024, "max_cost_microusd": 0, "timeout_ms": 1000,
	}
	if mutate != nil {
		mutate(body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// seedChildParent 创建 root Echo/Run 并认领租约，返回已持有的 parent 记录。
// parent Run 携带两个 Capability 的授权，供 grant 排序回归使用多 grant 请求。
func seedChildParent(t *testing.T, store *sqlite.Store, config appconfig.Config, tag string) kernelecho.RunRecord {
	t.Helper()
	now := time.Now().UTC()
	_, parent := echoRunRecords("app", "echo-"+tag, "parent-"+tag, "task", now)
	parent.CapabilityGrants = config.CapabilityGrants
	fingerprint := strings.Repeat(tag[:1], 64)
	if _, created, err := store.CreateEchoRunIdempotentLimited(t.Context(), "create-"+tag, fingerprint, kernelecho.Record{
		ID: "echo-" + tag, AppID: "app", InputMessage: "task", Status: kernelecho.StatusRunning, CreatedAt: now,
	}, parent, 0); err != nil || !created {
		t.Fatalf("create parent err=%v created=%v", err, created)
	}
	claimed, err := store.ClaimRun(t.Context(), "app", "echo-"+tag, parent.ID, "lease-"+tag, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestCreateChildRejectsBudgetsExceedingUnlimitedCostOrTimeout(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, err := appconfig.Normalize(appconfig.Config{
		AppID: "app", Enabled: true, ExecutorID: "executor.test", ExecutorConfig: json.RawMessage(`{"strategy":"test"}`),
		MaxSteps: 4, MaxCapabilityCalls: 4, MaxExecutionUnits: 2000, MaxOutputBytes: 4096,
		MaxCostMicrousd: 0, ExecutionTimeout: 5 * time.Second,
		CapabilityGrants: []capability.Grant{childGrant("app", "grant-parent", "campus.bus.routes.list")},
	})
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	service, err := childrun.NewService(store, childPolicySource{config: config})
	if err != nil {
		t.Fatal(err)
	}
	claimed := seedChildParent(t, store, config, "budget")
	grants := []capability.Grant{childGrant("app", "grant-child", "campus.bus.routes.list")}
	// 父 Run 不限成本（MaxCostMicrousd=0）时，child 不得自设成本上限。
	if _, err := service.CreateChild(t.Context(), contracts.RequestContext{
		AppID: "app", EchoID: "echo-budget", RunID: claimed.ID, CallID: "call-cost", LeaseToken: claimed.LeaseToken,
	}, createChildPayload(grants, func(body map[string]any) { body["max_cost_microusd"] = 500 })); !errors.Is(err, childrun.ErrInvalidRequest) {
		t.Fatalf("positive cost under unlimited parent accepted: %v", err)
	}
	// timeout_ms 超过父 Run 的单次执行时限也必须在创建期拒绝。
	if _, err := service.CreateChild(t.Context(), contracts.RequestContext{
		AppID: "app", EchoID: "echo-budget", RunID: claimed.ID, CallID: "call-timeout", LeaseToken: claimed.LeaseToken,
	}, createChildPayload(grants, func(body map[string]any) { body["timeout_ms"] = 8000 })); !errors.Is(err, childrun.ErrInvalidRequest) {
		t.Fatalf("timeout beyond parent execution limit accepted: %v", err)
	}
}

func TestCreateChildNormalizesGrantOrderAndRejectsDuplicates(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-grants.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, err := appconfig.Normalize(appconfig.Config{
		AppID: "app", Enabled: true, ExecutorID: "executor.test", ExecutorConfig: json.RawMessage(`{"strategy":"test"}`),
		MaxSteps: 4, MaxCapabilityCalls: 4, MaxExecutionUnits: 2000, MaxOutputBytes: 4096,
		MaxCostMicrousd: 0, ExecutionTimeout: 5 * time.Second,
		CapabilityGrants: []capability.Grant{
			childGrant("app", "grant-parent-a", "campus.bus.routes.list"),
			childGrant("app", "grant-parent-b", "campus.bus.schedule.get"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	service, err := childrun.NewService(store, childPolicySource{config: config})
	if err != nil {
		t.Fatal(err)
	}
	claimed := seedChildParent(t, store, config, "grants")
	// 显式 ID 按字典序倒序给出：请求顺序不可控，attenuateGrants 必须按 ID
	// 排序后落库（持久层要求严格递增），否则 child 在 claim 后 recovery_failed。
	requested := []capability.Grant{
		func() capability.Grant { g := childGrant("app", "grant-10", "campus.bus.schedule.get"); return g }(),
		func() capability.Grant { g := childGrant("app", "grant-2", "campus.bus.routes.list"); return g }(),
	}
	result, err := service.CreateChild(t.Context(), contracts.RequestContext{
		AppID: "app", EchoID: "echo-grants", RunID: claimed.ID, CallID: "call-order", LeaseToken: claimed.LeaseToken,
	}, createChildPayload(requested, nil))
	if err != nil {
		t.Fatalf("out-of-order grant ids rejected: %v", err)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(result, &created); err != nil || created.RunID == "" {
		t.Fatalf("child result=%s err=%v", result, err)
	}
	child, err := store.GetRun(t.Context(), "app", created.RunID)
	if err != nil || len(child.CapabilityGrants) != 2 {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	if child.CapabilityGrants[0].ID != "grant-10" || child.CapabilityGrants[1].ID != "grant-2" {
		t.Fatalf("grants not sorted by id: %s, %s", child.CapabilityGrants[0].ID, child.CapabilityGrants[1].ID)
	}
	// 重复 ID 属于授权路径错误，必须直接拒绝而不是落库后报持久层错误。
	duplicated := []capability.Grant{
		childGrant("app", "grant-dup", "campus.bus.routes.list"),
		childGrant("app", "grant-dup", "campus.bus.schedule.get"),
	}
	if _, err := service.CreateChild(t.Context(), contracts.RequestContext{
		AppID: "app", EchoID: "echo-grants", RunID: claimed.ID, CallID: "call-dup", LeaseToken: claimed.LeaseToken,
	}, createChildPayload(duplicated, nil)); !errors.Is(err, childrun.ErrGrantDenied) {
		t.Fatalf("duplicate grant ids accepted: %v", err)
	}
}

func TestChildRunIsDurableAndDoesNotCompleteEcho(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	parentGrant := childGrant("app", "grant-parent", "campus.bus.routes.list")
	_, parent := echoRunRecords("app", "echo", "parent", "task", now)
	parent.CapabilityGrants = []capability.Grant{parentGrant}
	if _, created, err := store.CreateEchoRunIdempotentLimited(t.Context(), "create-parent", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", kernelecho.Record{
		ID: "echo", AppID: "app", InputMessage: "task", Status: kernelecho.StatusRunning, CreatedAt: now,
	}, parent, 0); err != nil || !created {
		t.Fatalf("create parent err=%v created=%v", err, created)
	}
	claimed, err := store.ClaimRun(t.Context(), "app", "echo", parent.ID, "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	config, err := appconfig.Normalize(appconfig.Config{
		AppID: "app", Enabled: true, ExecutorID: parent.ExecutorID, ExecutorConfig: parent.ExecutorConfig,
		MaxSteps: parent.MaxSteps, MaxCapabilityCalls: parent.MaxCapabilityCalls, MaxExecutionUnits: parent.MaxExecutionUnits,
		MaxOutputBytes: parent.MaxOutputBytes, MaxCostMicrousd: parent.MaxCostMicrousd,
		ExecutionTimeout: time.Duration(parent.ExecutionTimeoutMS) * time.Millisecond,
		CapabilityGrants: []capability.Grant{parentGrant},
	})
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	service, err := childrun.NewService(store, childPolicySource{config: config})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := childrun.Register(reg, service); err != nil {
		t.Fatalf("register child capabilities: %v", err)
	}
	requested := childGrant("app", "requested", "campus.bus.routes.list")
	requested.Resource.IDs = []string{"route-1"}
	requested.MaxCalls = 1
	payload, err := json.Marshal(map[string]any{
		"task": "inspect route", "capability_grants": []capability.Grant{requested},
		"max_steps": 2, "max_capability_calls": 2, "max_execution_units": 100,
		"max_output_bytes": 1024, "max_cost_microusd": 0, "timeout_ms": 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateCapabilityInput("run.create_child", payload); err != nil {
		t.Fatalf("validate child capability payload: %v", err)
	}
	result, err := service.CreateChild(t.Context(), contracts.RequestContext{
		AppID: "app", EchoID: "echo", RunID: claimed.ID, CallID: "create-child", LeaseToken: claimed.LeaseToken,
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(result, &created); err != nil || created.RunID == "" {
		t.Fatalf("child result=%s err=%v", result, err)
	}
	child, err := store.GetRun(t.Context(), "app", created.RunID)
	if err != nil || child.ParentRunID != claimed.ID || child.Status != kernelecho.RunStatusQueued || len(child.CapabilityGrants) != 1 {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	childStarted := time.Now().UTC()
	childClaimed, err := store.ClaimRun(t.Context(), "app", "echo", child.ID, "child-lease", childStarted, childStarted.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(t.Context(), childClaimed, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, kernelecho.Output{ContentType: "text/plain", Data: []byte("done")}, publicerror.Error{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	echo, _, err := store.GetEcho(t.Context(), "app", "echo")
	if err != nil || echo.Status != kernelecho.StatusRunning {
		t.Fatalf("child completion changed Echo: %#v err=%v", echo, err)
	}
}

func TestChildRunCreationRequiresRootLeaseAndEnforcesLimit(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "child-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	_, parent := echoRunRecords("app", "echo", "parent", "task", now)
	if _, created, err := store.CreateEchoRunIdempotentLimited(t.Context(), "create-parent", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", kernelecho.Record{
		ID: "echo", AppID: "app", InputMessage: "task", Status: kernelecho.StatusRunning, CreatedAt: now,
	}, parent, 0); err != nil || !created {
		t.Fatalf("create parent err=%v created=%v", err, created)
	}
	claimed, err := store.ClaimRun(t.Context(), "app", "echo", parent.ID, "parent-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	child := parent
	child.ID = "child"
	child.RunGroupID = "child"
	child.ParentRunID = parent.ID
	child.OriginCallID = "call"
	child.Status = kernelecho.RunStatusQueued
	child.CreatedAt = now
	child.AvailableAt = now
	child.Deadline = now.Add(time.Minute)
	if err := store.CreateChildRun(t.Context(), claimed, child, 1); err != nil {
		t.Fatal(err)
	}
	child.ID = "child-2"
	child.RunGroupID = "child-2"
	child.OriginCallID = "call-2"
	if !errors.Is(store.CreateChildRun(t.Context(), claimed, child, 1), kernelecho.ErrChildRunLimit) {
		t.Fatal("child limit was not enforced")
	}
	// 终态 child 不再占用并发治理容量：完成后同限额可继续创建。
	childClaimed, err := store.ClaimRun(t.Context(), "app", "echo", "child", "child-lease", now.Add(2*time.Second), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(t.Context(), childClaimed, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, kernelecho.Output{ContentType: "text/plain; charset=utf-8", Data: []byte("done")}, publicerror.Error{}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateChildRun(t.Context(), claimed, child, 1); err != nil {
		t.Fatalf("terminal child still consumed capacity: %v", err)
	}
	claimed.LeaseToken = "wrong"
	child.ID = "child-3"
	child.RunGroupID = "child-3"
	child.OriginCallID = "call-3"
	if !errors.Is(store.CreateChildRun(t.Context(), claimed, child, 4), kernelecho.ErrInvalidTransition) {
		t.Fatal("invalid parent lease was accepted")
	}
}
