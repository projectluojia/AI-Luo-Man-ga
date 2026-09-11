// Package hostedtest 提供所有 hosted 包测试装配的共享机制：读仓库内真实包
// 清单（ailuo.toml）与源码，由 packagefmt go-wasm 构建器现场编译，ailuo.store
// 宿主函数绑定到 packstore 端口——清单与 guest 的测试即生产形态。
//
// 各包测试辅助（campustest/timetabletest/campustoolstest/...）只声明自己的
// 标识常量并调用 Register；装配机制不重复。
package hostedtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	kernelruntime "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
)

// Spec 描述一个被装配的 hosted 包：仓库内目录与测试夹具标识。
type Spec struct {
	// Dir 是仓库内包目录名（packages/<Dir>）。
	Dir string
	// ComponentID 是包内 provider 组件标识（与清单 components 一致）。
	ComponentID string
	// StorageNamespace 是清单 [storage] namespace 的期望值（防清单漂移）。
	StorageNamespace string
}

// PackageRoot 返回仓库内包目录（testsupport/hostedtest → 上溯两级到仓库根，
// 再进 packages/<spec.Dir>）。
func (s Spec) PackageRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate test source file")
	}
	// thisFile = <repo>/testsupport/hostedtest/hostedtest.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	root := filepath.Join(repoRoot, "packages", s.Dir)
	if info, err := os.Stat(filepath.Join(root, "ailuo.toml")); err != nil || info.IsDir() {
		return "", fmt.Errorf("%s package root not found near %s", s.Dir, thisFile)
	}
	return root, nil
}

// compiledCache 是现场编译的 guest 工件缓存（进程内每包只编译一次）。
var compiledCache sync.Map // dir -> *compiled

type compiled struct {
	once     sync.Once
	artifact []byte
	err      error
}

// Register 以 hosted 包形态装配包：真实清单 + 真实 guest 源码，ailuo.store
// 宿主函数绑定到 packstore 端口，与生产安装包链路一致。
func Register(t testing.TB, target *registry.Registry, store packstore.Store, spec Spec) {
	t.Helper()
	artifact := buildGuest(t, spec)
	digest := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(digest[:])

	root, err := spec.PackageRoot()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		t.Fatalf("parse %s ailuo.toml: %v", spec.Dir, err)
	}
	if manifest.Storage == nil || manifest.Storage.Namespace != spec.StorageNamespace {
		t.Fatalf("unexpected manifest id=%q storage=%#v", manifest.ID, manifest.Storage)
	}
	loaderManifest := loader.Manifest{
		ID: manifest.ID, PackageID: manifest.ID, Version: manifest.Version, Mode: loader.ModeHosted,
		Role: loader.RoleProvider, LockedDigest: artifactDigest, Pin: true,
		ABIVersion:    manifest.Components[0].ABIVersion,
		Storage:       manifest.Storage,
		Capabilities:  manifest.Capabilities,
		HostFunctions: manifest.Components[0].HostFunctions,
	}
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, m loader.Manifest) ([]byte, error) {
			if m.ID != loaderManifest.ID || m.LockedDigest != loaderManifest.LockedDigest {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
		HostFunctionsFor: func(m loader.Manifest) ([]loader.HostedFunction, error) {
			return packstore.ManifestFunctions(store, m)
		},
		RequireHostFunctions: true,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	record := loader.InstalledRecord{
		Runtime:   loaderManifest,
		PackageID: manifest.ID, ComponentID: spec.ComponentID,
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownContext); err != nil {
			t.Errorf("loader shutdown: %v", err)
		}
	})
	if err := loader.RegisterInstalled(context.Background(), manager, target, []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	warmupContext, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := manager.Warmup(warmupContext, []string{manifest.ID}, 1); err != nil {
		t.Fatalf("warm %s hosted package: %v", spec.Dir, err)
	}
}

// buildGuest 现场编译真实包源码（整个测试进程每包只编译一次）。
func buildGuest(t testing.TB, spec Spec) []byte {
	t.Helper()
	loaded, _ := compiledCache.LoadOrStore(spec.Dir, &compiled{})
	entry := loaded.(*compiled)
	entry.once.Do(func() {
		entry.artifact, entry.err = compileGuest(spec)
	})
	if entry.err != nil {
		t.Fatalf("build %s guest wasm: %v", spec.Dir, entry.err)
	}
	return entry.artifact
}

func compileGuest(spec Spec) ([]byte, error) {
	root, err := spec.PackageRoot()
	if err != nil {
		return nil, err
	}
	manifest, _, builds, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		return nil, fmt.Errorf("parse %s ailuo.toml: %w", spec.Dir, err)
	}
	if err := packagefmt.Build(context.Background(), root, manifest, builds); err != nil {
		return nil, fmt.Errorf("build guest wasm: %w", err)
	}
	artifact, err := os.ReadFile(filepath.Join(root, manifest.Components[0].Entrypoint))
	if err != nil {
		return nil, err
	}
	// 工件不入库（.gitignore 覆盖 *.wasm）；构建后立即清理保持工作区干净。
	_ = os.Remove(filepath.Join(root, manifest.Components[0].Entrypoint))
	return artifact, nil
}

// ---- Dispatcher 装配与调用辅助 ----

// memIdempotencyStore 是集成测试用的最小内存幂等存储：单进程、无并发争用，
// 只维护 Manager.Execute 所需的 claim/complete/replay 语义。
type memIdempotencyStore struct {
	records map[string]*idempotency.Record
}

func newMemIdempotencyStore() *memIdempotencyStore {
	return &memIdempotencyStore{records: map[string]*idempotency.Record{}}
}

func recordKey(appID, scope, key string) string { return appID + "\x00" + scope + "\x00" + key }

func (s *memIdempotencyStore) BeginIdempotent(_ context.Context, claim idempotency.Claim, now time.Time) (idempotency.Record, bool, error) {
	k := recordKey(claim.AppID, claim.Scope, claim.Key)
	if existing, ok := s.records[k]; ok {
		return *existing, false, nil
	}
	record := idempotency.Record{
		Operation:      claim.Operation,
		Status:         idempotency.StatusExecuting,
		LeaseToken:     claim.LeaseToken,
		LeaseExpiresAt: claim.LeaseExpiresAt,
		CreatedAt:      now,
	}
	s.records[k] = &record
	return record, true, nil
}

func (s *memIdempotencyStore) GetIdempotent(_ context.Context, appID, scope, key string) (idempotency.Record, error) {
	if existing, ok := s.records[recordKey(appID, scope, key)]; ok {
		return *existing, nil
	}
	return idempotency.Record{}, idempotency.ErrRecordNotFound
}

func (s *memIdempotencyStore) CompleteIdempotent(_ context.Context, claim idempotency.Claim, status string, result []byte, errorCode string, completedAt time.Time, expiresAt time.Time) error {
	k := recordKey(claim.AppID, claim.Scope, claim.Key)
	record, ok := s.records[k]
	if !ok {
		return idempotency.ErrRecordNotFound
	}
	record.Status = status
	record.Result = result
	record.ErrorCode = errorCode
	record.CompletedAt = &completedAt
	record.ExpiresAt = &expiresAt
	return nil
}

// acceptAllConfirmations 是测试确认验证器：非空 ConfirmationID 的调用放行。
type acceptAllConfirmations struct{}

func (acceptAllConfirmations) VerifyConfirmation(context.Context, kernelruntime.ConfirmationRequest) error {
	return nil
}

// MemoryStore 返回集成测试用的内存文档存储（packstore.Store 实现）。
func MemoryStore() packstore.Store {
	return memory.NewDocuments()
}

// RequestContext 构造集成测试的治理上下文：固定测试身份 user-1。
func RequestContext(appID, requestID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: appID, EchoID: "echo-1", RequestID: requestID, UserID: "user-1",
		Deadline: time.Now().Add(time.Minute),
	}
}

// NewDispatcher 以完整治理链路装配 runtime.Dispatcher：静态策略批量启用
// appID 的全部 capabilityIDs，内存幂等存储 + 放行确认验证器。
func NewDispatcher(t testing.TB, reg *registry.Registry, appID string, capabilityIDs []string) *kernelruntime.Dispatcher {
	t.Helper()
	policy := runtimetest.NewStaticAppPolicy()
	for _, capabilityID := range capabilityIDs {
		policy.Enable(appID, capabilityID)
	}
	return kernelruntime.NewDispatcher(reg, policy, kernelruntime.DispatcherConfig{
		IdempotencyStore:     newMemIdempotencyStore(),
		ConfirmationVerifier: acceptAllConfirmations{},
	})
}

// Invoke 走真实 Dispatcher 前置治理链路：校验 → 策略 → 幂等/确认门槛。
// 固定测试身份 user-1；出错时返回 errText。
func Invoke(t testing.TB, d *kernelruntime.Dispatcher, appID, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	request := RequestContext(appID, "request-"+capabilityID)
	request.IdempotencyKey = idempotencyKey
	request.ConfirmationID = confirmationID
	result, err := d.InvokeCapability(t.Context(), request, capabilityID, json.RawMessage(payload))
	if err != nil {
		return false, nil, err.Error()
	}
	return true, result, ""
}

// AuthoritativeMeta 是 packstore 侧的权威快照元数据（与 guestkit.Govern 对齐）。
func AuthoritativeMeta(now time.Time) packstore.SnapshotMeta {
	return packstore.SnapshotMeta{
		Revision: "rev-1", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
}

// MustDoc 编码一个 packstore 快照文档。
func MustDoc(t testing.TB, id string, payload any) packstore.Document {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return packstore.Document{ID: id, Payload: data}
}
