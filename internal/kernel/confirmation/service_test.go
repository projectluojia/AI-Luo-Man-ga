package confirmation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

// fakeClock 是测试用的确定性时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func openService(t *testing.T) (*confirmation.Service, *sqlite.Store, *fakeClock) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "confirmation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	seedEchoRun(t, store, "app", "echo", "run")
	clock := newFakeClock()
	return confirmation.NewService(store, confirmation.Config{Now: clock.current}), store, clock
}

func seedEchoRun(t *testing.T, store *sqlite.Store, appID, echoID, runID string) {
	t.Helper()
	now := time.Now().UTC()
	_, _, err := store.CreateEchoRunIdempotentLimited(context.Background(), "confirmation-"+echoID, idempotency.Fingerprint([]byte("test-input")), kernelecho.Record{
		ID: echoID, AppID: appID, InputMessage: "test-input",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: runID, RunGroupID: runID, AppID: appID, EchoID: echoID,
		Attempt: 1, Status: kernelecho.RunStatusQueued,
		Model: "test-model", ModelConfigVersion: "v1", ProtocolVersion: "1.0",
		MaxSteps: 8, MaxToolCalls: 4, MaxInputTokens: 4096, MaxOutputTokens: 2048,
		MaxTotalTokens: 8192, MaxOutputBytes: 65536, MaxCostMicrousd: 0,
		ProviderTimeoutMS: 5000,
		Deadline:          now.Add(time.Hour),
		AvailableAt:       now,
		CreatedAt:         now,
		RecoverableState:  json.RawMessage(`{}`),
	}, 0)
	if err != nil {
		t.Fatalf("seed echo/run: %v", err)
	}
}

func requestConfirmation(
	t *testing.T,
	service *confirmation.Service,
	appID, echoID, runID, callID string,
	spec confirmation.RequestSpec,
	arguments string,
	expiresAt time.Time,
) confirmation.Confirmation {
	t.Helper()
	record, err := service.Request(context.Background(), appID, echoID, runID, callID, spec, []byte(arguments), expiresAt)
	if err != nil {
		t.Fatalf("request confirmation: %v", err)
	}
	return record
}

func verifyRequest(record confirmation.Confirmation) runtime.ConfirmationRequest {
	return runtime.ConfirmationRequest{
		AppID:          record.AppID,
		EchoID:         record.EchoID,
		RunID:          record.RunID,
		ConfirmationID: record.ConfirmationID,
		TargetType:     record.TargetType,
		TargetID:       record.TargetID,
		SideEffect:     record.SideEffect,
		IdempotencyKey: record.IdempotencyKey,
	}
}

func TestServiceRequestCreatesWaitingConfirmationWithDigest(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, time.Time{})

	if record.Status != confirmation.StatusWaiting || record.ConfirmedBy != "" || record.DecidedAt != nil {
		t.Fatalf("new confirmation must be waiting: %#v", record)
	}
	if err := confirmation.ValidateArgumentDigest(record.ArgumentDigest); err != nil {
		t.Fatalf("argument digest invalid: %v", err)
	}
	if !record.ExpiresAt.After(clock.current()) {
		t.Fatalf("default lifetime must place expiry in the future: %v", record.ExpiresAt)
	}
	if record.CapabilityID != "campus.bus.notify" {
		t.Fatalf("capability target must default capability_id to target_id: %q", record.CapabilityID)
	}
	resolved, err := service.Resolve(context.Background(), "app", record.ConfirmationID)
	if err != nil {
		t.Fatalf("resolve waiting confirmation: %v", err)
	}
	if resolved != record {
		t.Fatalf("resolved record differs: %#v != %#v", resolved, record)
	}
	// 参数不同则摘要不同：同一目标不同参数必须生成不同的确认。
	other := requestConfirmation(t, service, "app", "echo", "run", "call-2",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-2",
		},
		`{"message":"取消提醒"}`, time.Time{})
	if other.ArgumentDigest == record.ArgumentDigest {
		t.Fatal("changed arguments must change the digest")
	}
}

func TestServiceConfirmationSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "confirmation-reopen.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedEchoRun(t, store, "app", "echo", "run")
	clock := newFakeClock()
	service := confirmation.NewService(store, confirmation.Config{Now: clock.current})
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 用同一 SQLite 文件重新打开：待确认状态必须仍然存在（持久化，非内存）。
	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	serviceAfter := confirmation.NewService(reopened, confirmation.Config{Now: clock.current})
	resolved, err := serviceAfter.Resolve(context.Background(), "app", record.ConfirmationID)
	if err != nil {
		t.Fatalf("resolve after reopen: %v", err)
	}
	if resolved.Status != confirmation.StatusWaiting || resolved.ArgumentDigest != record.ArgumentDigest ||
		resolved.TargetID != record.TargetID || resolved.EchoID != record.EchoID || resolved.RunID != record.RunID {
		t.Fatalf("confirmation state lost across reopen: %#v", resolved)
	}
	// 重开后仍可正常决策。
	if _, err := serviceAfter.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide after reopen: %v", err)
	}
	if err := serviceAfter.VerifyConfirmation(context.Background(), verifyRequest(record)); err != nil {
		t.Fatalf("verify after reopen+decide: %v", err)
	}
}

func TestServiceVerifyApprovedSucceedsAndRejectsNonApproved(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))

	// waiting 状态不可执行。
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(record)); !errors.Is(err, confirmation.ErrNotApproved) {
		t.Fatalf("waiting verify got %v, want ErrNotApproved", err)
	}
	approved, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute))
	if err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	if approved.Status != confirmation.StatusApproved || approved.ConfirmedBy != "user-1" || approved.DecidedAt == nil {
		t.Fatalf("approved record incomplete: %#v", approved)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(record)); err != nil {
		t.Fatalf("approved verify got %v, want nil", err)
	}

	// 拒绝状态不可执行。
	rejected := requestConfirmation(t, service, "app", "echo", "run", "call-2",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-2",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", rejected.ConfirmationID,
		confirmation.StatusRejected, "user-2", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide reject: %v", err)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(rejected)); !errors.Is(err, confirmation.ErrNotApproved) {
		t.Fatalf("rejected verify got %v, want ErrNotApproved", err)
	}
}

func TestServiceVerifyRejectsScopeMismatch(t *testing.T) {
	t.Parallel()
	service, store, clock := openService(t)
	seedEchoRun(t, store, "other", "echo-other", "run-other")
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	// 跨 App 的 Verify 读不到记录：Store.Get 按 (app_id, confirmation_id) 作用域读取，
	// App 隔离在读取层已保证，返回 ErrNotFound（fail-closed，不泄露记录是否存在）。
	crossApp := verifyRequest(record)
	crossApp.AppID = "other"
	if err := service.VerifyConfirmation(context.Background(), crossApp); !errors.Is(err, confirmation.ErrNotFound) {
		t.Fatalf("跨 App got %v, want ErrNotFound", err)
	}
	for name, mutate := range map[string]func(*runtime.ConfirmationRequest){
		"跨 Capability": func(r *runtime.ConfirmationRequest) { r.TargetID = "other.capability" },
		"跨目标类型":        func(r *runtime.ConfirmationRequest) { r.TargetType = confirmation.TargetTypeTool },
		"跨 Echo":       func(r *runtime.ConfirmationRequest) { r.EchoID = "echo-other" },
		"跨 Run":        func(r *runtime.ConfirmationRequest) { r.RunID = "run-other" },
		"跨副作用类型":       func(r *runtime.ConfirmationRequest) { r.SideEffect = confirmation.SideEffectWrite },
		"跨幂等键":         func(r *runtime.ConfirmationRequest) { r.IdempotencyKey = "operation-other" },
	} {
		request := verifyRequest(record)
		mutate(&request)
		if err := service.VerifyConfirmation(context.Background(), request); !errors.Is(err, confirmation.ErrScopeMismatch) {
			t.Fatalf("%s got %v, want ErrScopeMismatch", name, err)
		}
	}
	// 跨 App 读不到记录：App 隔离在读取层已经保证。
	if _, err := service.Resolve(context.Background(), "other", record.ConfirmationID); !errors.Is(err, confirmation.ErrNotFound) {
		t.Fatalf("cross-app resolve got %v, want ErrNotFound", err)
	}
}

func TestServiceVerifyRejectsExpiredAndRevoked(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)

	expired := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(5*time.Minute))
	if _, err := service.Decide(context.Background(), "app", expired.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	// 显式过期后验证失败。
	if _, err := service.Expire(context.Background(), "app", expired.ConfirmationID, clock.current().Add(2*time.Minute)); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(expired)); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("expired verify got %v, want ErrExpired", err)
	}

	// 有效期已过但状态机未显式标记：验证仍必须失败（fail-closed 时间过期）。
	lapsed := requestConfirmation(t, service, "app", "echo", "run", "call-2",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-2",
		},
		`{"message":"发车提醒"}`, clock.current().Add(5*time.Minute))
	if _, err := service.Decide(context.Background(), "app", lapsed.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	clock.advance(10 * time.Minute)
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(lapsed)); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("lapsed verify got %v, want ErrExpired", err)
	}
	if _, err := service.Resolve(context.Background(), "app", lapsed.ConfirmationID); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("lapsed resolve got %v, want ErrExpired", err)
	}

	// 撤销后验证失败；重复撤销幂等成功。
	revoked := requestConfirmation(t, service, "app", "echo", "run", "call-3",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-3",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", revoked.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current()); err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	if _, err := service.Revoke(context.Background(), "app", revoked.ConfirmationID, clock.current().Add(time.Second)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(revoked)); !errors.Is(err, confirmation.ErrRevoked) {
		t.Fatalf("revoked verify got %v, want ErrRevoked", err)
	}
	if _, err := service.Revoke(context.Background(), "app", revoked.ConfirmationID, clock.current().Add(2*time.Second)); err != nil {
		t.Fatalf("repeated revoke got %v, want idempotent nil", err)
	}
}

func TestServiceDecideConflictAndExpirySemantics(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))

	// 重复批准：幂等成功，返回同一个已批准记录，不重复创建或覆盖。
	first, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute))
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	repeated, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-2", clock.current().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("repeated approve must be idempotent: %v", err)
	}
	if repeated.Status != confirmation.StatusApproved || repeated.ConfirmedBy != "user-1" ||
		!repeated.DecidedAt.Equal(*first.DecidedAt) {
		t.Fatalf("repeated approve changed the record: %#v", repeated)
	}
	// 批准后拒绝：冲突。
	if _, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusRejected, "user-3", clock.current().Add(3*time.Minute)); !errors.Is(err, confirmation.ErrAlreadyDecided) {
		t.Fatalf("approve-then-reject got %v, want ErrAlreadyDecided", err)
	}

	// 拒绝后重复拒绝幂等；拒绝后批准冲突。
	rejected := requestConfirmation(t, service, "app", "echo", "run", "call-2",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-2",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", rejected.ConfirmationID,
		confirmation.StatusRejected, "user-4", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	if _, err := service.Decide(context.Background(), "app", rejected.ConfirmationID,
		confirmation.StatusRejected, "user-5", clock.current().Add(2*time.Minute)); err != nil {
		t.Fatalf("repeated reject must be idempotent: %v", err)
	}
	if _, err := service.Decide(context.Background(), "app", rejected.ConfirmationID,
		confirmation.StatusApproved, "user-6", clock.current().Add(3*time.Minute)); !errors.Is(err, confirmation.ErrAlreadyDecided) {
		t.Fatalf("reject-then-approve got %v, want ErrAlreadyDecided", err)
	}

	// 有效期已过的待确认记录不可再决策。
	short := requestConfirmation(t, service, "app", "echo", "run", "call-3",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-3",
		},
		`{"message":"发车提醒"}`, clock.current().Add(5*time.Minute))
	clock.advance(10 * time.Minute)
	if _, err := service.Decide(context.Background(), "app", short.ConfirmationID,
		confirmation.StatusApproved, "user-7", clock.current()); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("decide after expiry got %v, want ErrExpired", err)
	}
	// 已过期记录的撤销与再次决策都必须被拒绝。
	if _, err := service.Revoke(context.Background(), "app", short.ConfirmationID, clock.current()); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("revoke after expiry got %v, want ErrExpired", err)
	}
}

func TestServiceRevokeRunInvalidatesConfirmations(t *testing.T) {
	t.Parallel()
	service, store, clock := openService(t)
	seedEchoRun(t, store, "app", "echo-2", "run-2")
	spec := confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
		IdempotencyKey: "operation-1",
	}
	waiting := requestConfirmation(t, service, "app", "echo", "run", "call-1", spec, `{}`, clock.current().Add(time.Hour))
	approved := requestConfirmation(t, service, "app", "echo", "run", "call-2", spec, `{}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", approved.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	otherRun := requestConfirmation(t, service, "app", "echo-2", "run-2", "call-3", spec, `{}`, clock.current().Add(time.Hour))

	// Run 取消：该 Run 下的全部等待/批准确认失效，其他 Run 不受影响。
	affected, err := service.RevokeRun(context.Background(), "app", "run", clock.current().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("revoke run: %v", err)
	}
	if affected != 2 {
		t.Fatalf("revoked count=%d, want 2", affected)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(waiting)); !errors.Is(err, confirmation.ErrRevoked) {
		t.Fatalf("waiting after run revoke got %v, want ErrRevoked", err)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(approved)); !errors.Is(err, confirmation.ErrRevoked) {
		t.Fatalf("approved after run revoke got %v, want ErrRevoked", err)
	}
	if err := service.VerifyConfirmation(context.Background(), verifyRequest(otherRun)); !errors.Is(err, confirmation.ErrNotApproved) {
		t.Fatalf("other-run confirmation got %v, want ErrNotApproved", err)
	}
	// 已撤销确认无法再批准。
	if _, err := service.Decide(context.Background(), "app", waiting.ConfirmationID,
		confirmation.StatusApproved, "user-2", clock.current().Add(3*time.Minute)); !errors.Is(err, confirmation.ErrRevoked) {
		t.Fatalf("decide revoked got %v, want ErrRevoked", err)
	}
}

func TestServiceExpireDueBatchExpiresOnlyDueConfirmations(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)
	spec := confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
		IdempotencyKey: "operation-1",
	}
	due := requestConfirmation(t, service, "app", "echo", "run", "call-1", spec, `{}`, clock.current().Add(5*time.Minute))
	future := requestConfirmation(t, service, "app", "echo", "run", "call-2", spec, `{}`, clock.current().Add(time.Hour))
	clock.advance(10 * time.Minute)
	affected, err := service.ExpireDue(context.Background(), "app", clock.current())
	if err != nil {
		t.Fatalf("expire due: %v", err)
	}
	if affected != 1 {
		t.Fatalf("expired count=%d, want 1", affected)
	}
	if _, err := service.Resolve(context.Background(), "app", due.ConfirmationID); !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("due confirmation got %v, want ErrExpired", err)
	}
	if resolved, err := service.Resolve(context.Background(), "app", future.ConfirmationID); err != nil || resolved.Status != confirmation.StatusWaiting {
		t.Fatalf("future confirmation resolved=%#v err=%v, want waiting", resolved, err)
	}
}

func TestServiceDoesNotFabricateUnknownExecutionOutcome(t *testing.T) {
	t.Parallel()
	service, store, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))
	approved, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute))
	if err != nil {
		t.Fatalf("decide approve: %v", err)
	}

	// 状态机只承认 waiting/approved/rejected/expired/revoked：
	// 执行结果既不是状态，也永远不被确认状态机伪报为成功或失败。
	for _, fabricated := range []string{"succeeded", "failed", "executed"} {
		if err := confirmation.ValidateStatus(fabricated); err == nil {
			t.Fatalf("状态机不得伪报执行结果状态 %q", fabricated)
		}
	}

	// 副作用执行从未上报结果（执行进程在完成后崩溃前中断）：
	// 确认不得伪报失败，也不得自动把"未知"变成成功。
	resolved, err := service.Resolve(context.Background(), "app", approved.ConfirmationID)
	if err != nil {
		t.Fatalf("resolve without outcome: %v", err)
	}
	if resolved.Status != confirmation.StatusApproved {
		t.Fatalf("confirmation must stay approved without fabricated result: %q", resolved.Status)
	}

	// 幂等层对未知执行结果返回 ErrOutcomeUnknown（不自动重试、不伪报成功/失败），
	// 确认状态不被任何执行结果字段污染。
	manager := idempotency.NewManager(store)
	operation := idempotency.Operation{
		AppID: "app", Scope: "runtime.capability/campus.bus.notify/1.0.0",
		Key: "operation-1", Fingerprint: idempotency.Fingerprint([]byte(`{"message":"发车提醒"}`)),
		OwnerID: "request",
	}
	now := time.Now().UTC()
	claim := idempotency.Claim{
		Operation: operation, LeaseToken: "lease-1", LeaseExpiresAt: now.Add(150 * time.Millisecond),
	}
	if _, claimed, err := store.BeginIdempotent(context.Background(), claim, now); err != nil || !claimed {
		t.Fatalf("pre-claim side effect: claimed=%v err=%v", claimed, err)
	}
	time.Sleep(250 * time.Millisecond)
	executions := 0
	_, _, err = manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
		executions++
		return []byte(`{}`), nil
	})
	if !errors.Is(err, idempotency.ErrOutcomeUnknown) {
		t.Fatalf("unknown outcome got %v, want ErrOutcomeUnknown", err)
	}
	if executions != 0 {
		t.Fatal("未知执行结果不得自动重试副作用")
	}
	if public := publicerror.Capability(err); public.Code != "idempotency_outcome_unknown" {
		t.Fatalf("public error code=%s, want idempotency_outcome_unknown", public.Code)
	}
	// 未知结果不影响确认自身的授权状态：仍是 approved，未伪造任何结果。
	resolved, err = service.Resolve(context.Background(), "app", approved.ConfirmationID)
	if err != nil {
		t.Fatalf("resolve after unknown outcome: %v", err)
	}
	if resolved.Status != confirmation.StatusApproved {
		t.Fatalf("confirmation status was corrupted by unknown outcome: %q", resolved.Status)
	}
}

func TestServiceConcurrentRepeatedApprovalIsIdempotent(t *testing.T) {
	service, _, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
			TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"message":"发车提醒"}`, clock.current().Add(time.Hour))

	ctx := context.Background()
	const workers = 16
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = service.Decide(ctx, "app", record.ConfirmationID,
				confirmation.StatusApproved, fmt.Sprintf("user-%d", index), clock.current())
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d got %v, want idempotent nil", index, err)
		}
	}
	resolved, err := service.Resolve(ctx, "app", record.ConfirmationID)
	if err != nil {
		t.Fatalf("resolve after concurrent approval: %v", err)
	}
	if resolved.Status != confirmation.StatusApproved {
		t.Fatalf("concurrent approval left status=%q, want approved", resolved.Status)
	}
	if err := service.VerifyConfirmation(ctx, verifyRequest(record)); err != nil {
		t.Fatalf("verify after concurrent approval: %v", err)
	}
}

func TestServiceRequestRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	service, _, clock := openService(t)
	ctx := context.Background()
	spec := confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal,
		IdempotencyKey: "operation-1",
	}
	if _, err := service.Request(ctx, "", "echo", "run", "call-1", spec, nil, clock.current().Add(time.Hour)); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("empty app got %v, want ErrInvalidRequest", err)
	}
	if _, err := service.Request(ctx, "app", "echo", "run", "call-1", confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: "database",
		TargetID: "x", SideEffect: confirmation.SideEffectExternal, IdempotencyKey: "operation-1",
	}, nil, clock.current().Add(time.Hour)); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("invalid target type got %v, want ErrInvalidRequest", err)
	}
	// 只读副作用不需要确认，必须在请求边界拒绝（确认仅治理 write/external）。
	if _, err := service.Request(ctx, "app", "echo", "run", "call-1", confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: capability.SideEffectRead, IdempotencyKey: "operation-1",
	}, nil, clock.current().Add(time.Hour)); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("read side effect got %v, want ErrInvalidRequest", err)
	}
	if _, err := service.Request(ctx, "app", "echo", "run", "call-1", confirmation.RequestSpec{
		CapabilityID: "campus.bus.notify", TargetType: confirmation.TargetTypeCapability,
		TargetID: "campus.bus.notify", SideEffect: confirmation.SideEffectExternal, IdempotencyKey: "bad key!",
	}, nil, clock.current().Add(time.Hour)); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("invalid idempotency key got %v, want ErrInvalidRequest", err)
	}
	if _, err := service.Request(ctx, "app", "echo", "run", "call-1", spec,
		[]byte(`{"broken":`), clock.current().Add(time.Hour)); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("invalid arguments got %v, want ErrInvalidRequest", err)
	}
	if _, err := service.Request(ctx, "app", "echo", "run", "call-1", spec,
		nil, clock.current()); !errors.Is(err, confirmation.ErrInvalidRequest) {
		t.Fatalf("non-future expiry got %v, want ErrInvalidRequest", err)
	}
}

func registerCapability(t *testing.T, reg *registry.Registry, spec capability.CapabilitySpec, handler registry.Handler) {
	t.Helper()
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: capability.ServiceSpec{
			ID:                   spec.ServiceID,
			Version:              "1.0.0",
			RequestedPermissions: append([]string(nil), spec.RequiredPermissions...),
		},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			spec.ID: {Spec: spec, Handler: handler},
		},
	}); err != nil {
		t.Fatalf("register capability: %v", err)
	}
}

func validRequest(runID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID:     "app",
		EchoID:    "echo",
		RunID:     runID,
		RequestID: "request",
		Deadline:  time.Now().Add(time.Minute),
	}
}

func TestDispatcherExecutesApprovedSideEffectExactlyOnce(t *testing.T) {
	service, store, clock := openService(t)
	record := requestConfirmation(t, service, "app", "echo", "run", "call-1",
		confirmation.RequestSpec{
			CapabilityID: "external-capability", TargetType: confirmation.TargetTypeCapability,
			TargetID: "external-capability", SideEffect: confirmation.SideEffectExternal,
			IdempotencyKey: "operation-1",
		},
		`{"value":1}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", record.ConfirmationID,
		confirmation.StatusApproved, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide approve: %v", err)
	}

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "external-capability")
	executions := 0
	registerCapability(t, reg, capability.CapabilitySpec{
		ID:                   "external-capability",
		Version:              "1.0.0",
		ServiceID:            "service",
		InputSchemaJSON:      `{"type":"object","properties":{"value":{"type":"integer"}},"additionalProperties":false}`,
		SideEffect:           capability.SideEffectExternal,
		RequiresConfirmation: true,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"sent":true}`), nil
	})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: service,
	})

	request := validRequest("run")
	request.ConfirmationID = record.ConfirmationID
	request.IdempotencyKey = "operation-1"

	// 已批准：副作用恰好执行一次。
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	if executions != 1 {
		t.Fatalf("executions=%d, want 1", executions)
	}
	// 重复调用（同键同参数）：幂等重放，副作用不重复执行。
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatalf("replayed invoke: %v", err)
	}
	if executions != 1 {
		t.Fatalf("executions after replay=%d, want 1", executions)
	}
	// 参数改变（同键不同参数）：幂等指纹冲突拒绝执行，旧确认不可复用。
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{"value":2}`)); !errors.Is(err, idempotency.ErrKeyConflict) {
		t.Fatalf("changed arguments got %v, want ErrKeyConflict", err)
	}
	if executions != 1 {
		t.Fatalf("executions after conflict=%d, want 1", executions)
	}
}

func TestDispatcherRejectsUnapprovedConfirmation(t *testing.T) {
	service, store, clock := openService(t)
	spec := confirmation.RequestSpec{
		CapabilityID: "external-capability", TargetType: confirmation.TargetTypeCapability,
		TargetID: "external-capability", SideEffect: confirmation.SideEffectExternal,
		IdempotencyKey: "operation-1",
	}
	waiting := requestConfirmation(t, service, "app", "echo", "run", "call-1", spec, `{"value":1}`, clock.current().Add(time.Hour))
	rejected := requestConfirmation(t, service, "app", "echo", "run", "call-2", spec, `{"value":1}`, clock.current().Add(time.Hour))
	if _, err := service.Decide(context.Background(), "app", rejected.ConfirmationID,
		confirmation.StatusRejected, "user-1", clock.current().Add(time.Minute)); err != nil {
		t.Fatalf("decide reject: %v", err)
	}

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "external-capability")
	registerCapability(t, reg, capability.CapabilitySpec{
		ID:                   "external-capability",
		Version:              "1.0.0",
		ServiceID:            "service",
		InputSchemaJSON:      `{"type":"object","additionalProperties":false}`,
		SideEffect:           capability.SideEffectExternal,
		RequiresConfirmation: true,
	}, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		t.Error("unapproved capability must never execute")
		return json.RawMessage(`{}`), nil
	})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: service,
	})

	for name, confirmationID := range map[string]string{
		"等待中":    waiting.ConfirmationID,
		"已拒绝":    rejected.ConfirmationID,
		"不存在的确认": strings.Repeat("f", 8),
	} {
		request := validRequest("run")
		request.ConfirmationID = confirmationID
		request.IdempotencyKey = "operation-1"
		if _, err := dispatcher.InvokeCapability(context.Background(), request, "external-capability", json.RawMessage(`{"value":1}`)); !errors.Is(err, runtime.ErrConfirmationRequired) {
			t.Fatalf("%s got %v, want ErrConfirmationRequired", name, err)
		}
	}
}
