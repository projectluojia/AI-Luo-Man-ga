// Package childrun 提供所有 Executor 共用的受治理 child Run Capability。
package childrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	MaxChildren  = 4
	maxTaskBytes = 16384
)

var (
	ErrInvalidRequest = errors.New("invalid child Run request")
	ErrNotRoot        = errors.New("only a root Run may create child Runs")
	ErrGrantDenied    = errors.New("child Run Grant is not a subset of the parent")
)

type Store interface {
	echo.ChildRunCreationStore
	GetRun(context.Context, string, string) (echo.RunRecord, error)
}

type Service struct {
	store  Store
	policy appconfig.Source
	now    func() time.Time
}

func NewService(store Store, policy appconfig.Source) (*Service, error) {
	if store == nil || policy == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{store: store, policy: policy, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Register 将 Core child Run 能力注册到统一 Registry；它不依赖任何特定 Executor。
func Register(reg *registry.Registry, service *Service) error {
	if reg == nil || service == nil {
		return ErrInvalidRequest
	}
	return reg.RegisterBatch([]registry.CapabilityRegistration{
		{Spec: createSpec(), Handler: service.CreateChild},
		{Spec: statusSpec(), Handler: service.statusHandler},
	})
}

func createSpec() capability.CapabilitySpec {
	return capability.CapabilitySpec{
		ID: "run.create_child", Version: "1.0.0", Name: "创建子 Run",
		Description:     "创建一个由 Core 持久化、独立调度和可取消的直接 child Run",
		InputSchemaJSON: `{"type":"object","required":["task","capability_grants","max_steps","max_capability_calls","max_execution_units","max_output_bytes","max_cost_microusd","timeout_ms"],"additionalProperties":false,"properties":{"task":{"type":"string","minLength":1,"maxLength":16384},"capability_grants":{"type":"array","maxItems":64,"items":{"type":"object","required":["id","app_id","principal","capability_id","resource","expires_at","max_calls","max_cost_microusd","delegable","max_delegation_depth","policy_revision"],"additionalProperties":false,"properties":{"id":{"type":"string"},"app_id":{"type":"string"},"principal":{"type":"string"},"capability_id":{"type":"string"},"resource":{"type":"object","required":["type"],"additionalProperties":false,"properties":{"type":{"type":"string"},"ids":{"type":"array","items":{"type":"string"}},"relation":{"type":"string"}}},"not_before":{"type":["string","null"]},"expires_at":{"type":"string"},"max_calls":{"type":"integer","minimum":1},"max_cost_microusd":{"type":"integer","minimum":0},"audience":{"type":"string"},"delegable":{"type":"boolean"},"max_delegation_depth":{"type":"integer","minimum":0},"policy_revision":{"type":"string"}}}},"max_steps":{"type":"integer","minimum":1},"max_capability_calls":{"type":"integer","minimum":1},"max_execution_units":{"type":"integer","minimum":1},"max_output_bytes":{"type":"integer","minimum":1},"max_cost_microusd":{"type":"integer","minimum":0},"timeout_ms":{"type":"integer","minimum":100}}}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "run"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectState, Replay: capability.ReplayIdempotencyKey, ConfirmationFloor: capability.ConfirmationPolicy},
	}
}

func statusSpec() capability.CapabilitySpec {
	return capability.CapabilitySpec{
		ID: "run.get_child_status", Version: "1.0.0", Name: "读取子 Run 状态",
		Description:     "读取当前 root Run 直接 child 的持久状态和结果",
		InputSchemaJSON: `{"type":"object","required":["child_run_id"],"additionalProperties":false,"properties":{"child_run_id":{"type":"string","minLength":1,"maxLength":128}}}`,
		Authorization:   capability.AuthorizationSpec{ResourceType: "run", ResourceIDFrom: "/child_run_id"},
		Execution:       capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
	}
}

type createRequest struct {
	Task               string             `json:"task"`
	CapabilityGrants   []capability.Grant `json:"capability_grants"`
	MaxSteps           uint32             `json:"max_steps"`
	MaxCapabilityCalls uint32             `json:"max_capability_calls"`
	MaxExecutionUnits  uint64             `json:"max_execution_units"`
	MaxOutputBytes     uint64             `json:"max_output_bytes"`
	MaxCostMicrousd    uint64             `json:"max_cost_microusd"`
	TimeoutMS          uint32             `json:"timeout_ms"`
}

func (s *Service) CreateChild(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	var input createRequest
	if err := decode(payload, &input); err != nil || len(input.Task) == 0 || len([]byte(input.Task)) > maxTaskBytes ||
		input.MaxSteps == 0 || input.MaxCapabilityCalls == 0 || input.MaxExecutionUnits == 0 || input.MaxOutputBytes == 0 || input.TimeoutMS < 100 {
		return nil, ErrInvalidRequest
	}
	if request.RunID == "" || request.LeaseToken == "" || request.CallID == "" {
		return nil, ErrInvalidRequest
	}
	parent, err := s.store.GetRun(ctx, request.AppID, request.RunID)
	if err != nil {
		return nil, err
	}
	if parent.ParentRunID != "" || parent.EchoID != request.EchoID || parent.Status != echo.RunStatusRunning || parent.LeaseToken != request.LeaseToken {
		return nil, ErrNotRoot
	}
	now := s.now().UTC()
	if !parent.Deadline.After(now) {
		return nil, context.DeadlineExceeded
	}
	childID := "run-" + uuid.NewString()
	grants, err := s.attenuateGrants(ctx, request.AppID, parent, input.CapabilityGrants, childID)
	if err != nil {
		return nil, err
	}
	remainingUnits := parent.MaxExecutionUnits - parent.UsedExecutionUnits
	if parent.UsedExecutionUnits > parent.MaxExecutionUnits {
		return nil, ErrInvalidRequest
	}
	remainingCost := parent.MaxCostMicrousd
	costLimited := parent.MaxCostMicrousd != 0
	if costLimited {
		if parent.UsedCostMicrousd > parent.MaxCostMicrousd {
			return nil, ErrInvalidRequest
		}
		remainingCost -= parent.UsedCostMicrousd
	}
	// 预算按字段语义分两类校验：
	//   累积池（execution_units / cost）按父 Run 余额衰减——父 Run 已消费的
	//   部分不能再分配给 child；MaxCostMicrousd=0 表示不设成本上限，child
	//   继承该语义（父不限时 child 也不得自设上限，与 claim 复核一致）。
	//   步数与 Capability 调用数是单次执行的步进上限（每次 attempt 计数），
	//   按父上限收窄即可，树级总量由余额类预算和 deadline 约束。
	// 执行时限与成本同源：必须同时不超过父 Run 的单次执行时限，否则 child
	// 会在 claim 后被 runMatchesAppConfig 以 recovery_failed 拒绝。
	if input.MaxSteps > parent.MaxSteps || input.MaxCapabilityCalls > parent.MaxCapabilityCalls ||
		input.MaxExecutionUnits > remainingUnits || input.MaxOutputBytes > parent.MaxOutputBytes ||
		input.TimeoutMS > parent.ExecutionTimeoutMS ||
		costLimited && (input.MaxCostMicrousd == 0 || input.MaxCostMicrousd > remainingCost) ||
		!costLimited && input.MaxCostMicrousd != 0 {
		return nil, ErrInvalidRequest
	}
	deadline := now.Add(time.Duration(input.TimeoutMS) * time.Millisecond)
	if deadline.After(parent.Deadline) {
		return nil, ErrInvalidRequest
	}
	if !deadline.After(now) {
		return nil, context.DeadlineExceeded
	}
	child := echo.RunRecord{
		ID: childID, RunGroupID: "run-" + uuid.NewString(), AppID: parent.AppID, EchoID: parent.EchoID,
		ParentRunID: parent.ID, OriginCallID: request.CallID, SessionID: parent.SessionID,
		UserID: parent.UserID, MessageID: parent.MessageID, Channel: parent.Channel,
		Attempt: 1, Status: echo.RunStatusQueued, ExecutorID: parent.ExecutorID,
		ConfigRevision: parent.ConfigRevision, ProtocolVersion: parent.ProtocolVersion,
		ExecutorConfig: append(json.RawMessage(nil), parent.ExecutorConfig...),
		InputPayload:   []byte(input.Task), InputContentType: "text/plain; charset=utf-8",
		MaxSteps: input.MaxSteps, MaxCapabilityCalls: input.MaxCapabilityCalls,
		MaxExecutionUnits: input.MaxExecutionUnits, MaxOutputBytes: input.MaxOutputBytes,
		MaxCostMicrousd: input.MaxCostMicrousd, ExecutionTimeoutMS: input.TimeoutMS,
		Deadline: deadline, AvailableAt: now, CapabilityGrants: grants,
		RecoverableState: json.RawMessage(`{}`), CreatedAt: now,
	}
	if err := s.store.CreateChildRun(ctx, parent, child, MaxChildren); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"run_id": child.ID, "status": child.Status})
}

func (s *Service) statusHandler(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	var input struct {
		ChildRunID string `json:"child_run_id"`
	}
	if err := decode(payload, &input); err != nil || input.ChildRunID == "" || request.RunID == "" {
		return nil, ErrInvalidRequest
	}
	child, err := s.store.GetRun(ctx, request.AppID, input.ChildRunID)
	if err != nil {
		return nil, err
	}
	if child.ParentRunID != request.RunID || child.AppID != request.AppID {
		return nil, ErrGrantDenied
	}
	result := map[string]any{"run_id": child.ID, "status": child.Status}
	if child.ErrorCode != "" {
		result["error_code"] = child.ErrorCode
		result["error_message"] = child.ErrorMessage
	}
	if child.Status == echo.RunStatusSucceeded {
		result["result"] = map[string]string{
			"content_type": child.Result.ContentType,
			"data_base64":  base64.StdEncoding.EncodeToString(child.Result.Data),
		}
	}
	return json.Marshal(result)
}

func (s *Service) attenuateGrants(ctx context.Context, appID string, parent echo.RunRecord, requested []capability.Grant, audience string) ([]capability.Grant, error) {
	config, err := s.policy.Current(ctx, appID)
	if err != nil || appconfig.VerifyCurrent(config, appID) != nil || !config.Enabled {
		return nil, ErrGrantDenied
	}
	if len(requested) > 64 {
		return nil, ErrGrantDenied
	}
	grants := make([]capability.Grant, 0, len(requested))
	for index, candidate := range requested {
		candidate.Audience = audience
		if candidate.ID == "" {
			candidate.ID = fmt.Sprintf("grant-%s-%d", audience, index)
		}
		matched := false
		for _, parentGrant := range parent.CapabilityGrants {
			if parentGrant.CapabilityID != candidate.CapabilityID {
				continue
			}
			if narrowed, narrowErr := capability.NarrowGrant(parentGrant, candidate); narrowErr == nil {
				for _, policyGrant := range config.CapabilityGrants {
					if policyGrant.CapabilityID == candidate.CapabilityID {
						if _, policyErr := capability.NarrowGrant(policyGrant, narrowed); policyErr == nil {
							grants = append(grants, narrowed)
							matched = true
							break
						}
					}
				}
			}
			if matched {
				break
			}
		}
		if !matched || candidate.CapabilityID == "run.create_child" {
			return nil, ErrGrantDenied
		}
	}
	// 持久层要求 Grant ID 严格递增（validCanonicalGrants）；请求顺序不可控，
	// 这里按规范化后的 ID 排序，重复 ID 直接拒绝，保证 child 记录可持久化。
	sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
	for index := 1; index < len(grants); index++ {
		if grants[index].ID == grants[index-1].ID {
			return nil, ErrGrantDenied
		}
	}
	return grants, nil
}

func decode(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest
	}
	return nil
}
