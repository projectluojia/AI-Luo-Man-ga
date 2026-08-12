package campus_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
)

// campusWasmPath 是 campus hosted 工件路径（测试工作目录为 internal/campus）。
var campusWasmPath = filepath.Join("..", "..", "extensions", "campus", "campus.wasm")

func TestHostedCampusBuiltinManifestLocksArtifact(t *testing.T) {
	manifest := campus.Manifest()
	if manifest.ID != campus.ServiceID || manifest.Mode != loader.ModeHosted || len(manifest.LockedDigest) != 64 {
		t.Fatalf("manifest = %+v, want campus hosted with 64-hex digest", manifest)
	}
	artifact, err := campus.ReadArtifact(context.Background(), manifest)
	if err != nil || len(artifact) == 0 {
		t.Fatalf("ReadArtifact: %v", err)
	}
	// 清单与工件不一致必须被拒绝（防构建产物漂移）。
	bad := manifest
	bad.LockedDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := campus.ReadArtifact(context.Background(), bad); !errors.Is(err, loader.ErrDescribeMismatch) {
		t.Fatalf("ReadArtifact mismatched = %v, want ErrDescribeMismatch", err)
	}
}

func TestHostedCampusSpecsRegisterThroughInstalled(t *testing.T) {
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  campus.ReadArtifact,
		HostFunctions: campus.HostedFunctions(memory.NewBusStore()),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("loader shutdown: %v", err)
		}
	})
	reg := registry.New()
	record := loader.InstalledRecord{
		Runtime:      campus.Manifest(),
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
	}
	if err := loader.RegisterInstalled(manager, reg, []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	if len(reg.Tools()) != 3 || len(reg.Capabilities()) != 3 || len(reg.Services()) != 1 {
		t.Fatalf("registry = %d tools, %d capabilities, %d services; want 3/3/1",
			len(reg.Tools()), len(reg.Capabilities()), len(reg.Services()))
	}
	if _, err := manager.Snapshot(campus.ServiceID); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}

// TestHostedCampusCapabilityBehaviors 走完整 hosted 链路验证业务与治理行为：
// Dispatcher → Loader → WasmHost → campus guest → 宿主函数 → 权威存储。
func TestHostedCampusCapabilityBehaviors(t *testing.T) {
	baseTime := time.Date(2026, time.July, 24, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	store := memory.NewBusStore()
	// another-app 用于 App 隔离场景：两个 App 都启用 Capability，数据隔离由宿主函数强制。
	dispatcher := newHostedDispatcher(t, store, campus.AppID, "another-app")
	ctx := context.Background()
	invoke := func(appID string, payload json.RawMessage) (json.RawMessage, error) {
		return dispatcher.InvokeCapability(ctx, requestContext(appID), campus.BusJourneySearchCapabilityID, payload)
	}

	t.Run("journey ordering and filtering", func(t *testing.T) {
		store.Replace(campus.AppID, []bus.Journey{
			journey("trip-late", "stop-a", "stop-b", baseTime.Add(90*time.Minute)),
			journey("trip-before", "stop-a", "stop-b", baseTime.Add(-time.Minute)),
			journey("trip-early", "stop-a", "stop-b", baseTime.Add(30*time.Minute)),
			journey("trip-other-destination", "stop-a", "stop-c", baseTime.Add(10*time.Minute)),
		})
		payload := mustJSON(t, bus.SearchRequest{
			OriginStopID: "stop-a", DestinationStopID: "stop-b", DepartAfter: baseTime, Limit: 10,
		})
		resultPayload, err := invoke(campus.AppID, payload)
		if err != nil {
			t.Fatalf("invoke journey capability: %v", err)
		}
		var result bus.SearchResult
		if err := json.Unmarshal(resultPayload, &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if len(result.Journeys) != 2 || result.Journeys[0].TripID != "trip-early" || result.Journeys[1].TripID != "trip-late" {
			t.Fatalf("journeys = %#v, want trip-early then trip-late", result.Journeys)
		}
	})

	t.Run("empty result is governed array", func(t *testing.T) {
		store.Replace(campus.AppID, []bus.Journey{})
		payload := mustJSON(t, bus.SearchRequest{
			OriginStopID: "stop-a", DestinationStopID: "stop-b", DepartAfter: time.Now(),
		})
		resultPayload, err := invoke(campus.AppID, payload)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		var result bus.SearchResult
		if err := json.Unmarshal(resultPayload, &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if len(result.Journeys) != 0 || result.DataStatus.State != bus.DataStateAuthoritativeFresh {
			t.Fatalf("result = %s, want governed empty array", resultPayload)
		}
	})

	t.Run("app isolation", func(t *testing.T) {
		base := time.Now().Add(time.Hour)
		store.Replace(campus.AppID, []bus.Journey{journey("campus-trip", "stop-a", "stop-b", base)})
		payload := mustJSON(t, bus.SearchRequest{
			OriginStopID: "stop-a", DestinationStopID: "stop-b", DepartAfter: time.Now(),
		})
		if _, err := invoke("another-app", payload); !errors.Is(err, contracts.ErrDataUnavailable) {
			t.Fatalf("cross-App error = %v, want ErrDataUnavailable", err)
		}
	})

	t.Run("snapshot governance", func(t *testing.T) {
		now := time.Now().UTC()
		cases := []struct {
			name     string
			metadata bus.SnapshotMetadata
			target   error
		}{
			{
				name: "non-authoritative",
				metadata: bus.SnapshotMetadata{
					Revision: "demo", Source: "demo-copy", ImportedAt: now.Add(-time.Hour),
					ValidUntil: now.Add(time.Hour), Complete: true,
				},
				target: contracts.ErrDataUntrusted,
			},
			{
				name: "expired",
				metadata: bus.SnapshotMetadata{
					Revision: "expired", Source: "zhihui-luojia", Authoritative: true,
					ImportedAt: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Hour), Complete: true,
				},
				target: contracts.ErrDataExpired,
			},
			{
				name: "incomplete",
				metadata: bus.SnapshotMetadata{
					Revision: "incomplete", Source: "zhihui-luojia", Authoritative: true,
					ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
				},
				target: contracts.ErrDataIncomplete,
			},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				store.Replace(campus.AppID, []bus.Journey{})
				store.SetSnapshotMetadata(campus.AppID, tc.metadata)
				if _, err := invoke(campus.AppID, validPayload(t)); !errors.Is(err, tc.target) {
					t.Fatalf("error = %v, want %v", err, tc.target)
				}
			})
		}
	})

	t.Run("schema validation", func(t *testing.T) {
		store.Replace(campus.AppID, []bus.Journey{})
		store.SetSnapshotMetadata(campus.AppID, authoritativeFreshMetadata())
		payload := mustJSON(t, bus.SearchRequest{DestinationStopID: "stop-b"})
		if _, err := invoke(campus.AppID, payload); !errors.Is(err, registry.ErrSchemaValidation) {
			t.Fatalf("error = %v, want ErrSchemaValidation", err)
		}
	})

	t.Run("cancellation propagation", func(t *testing.T) {
		store.Replace(campus.AppID, []bus.Journey{})
		store.SetSnapshotMetadata(campus.AppID, authoritativeFreshMetadata())
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := dispatcher.InvokeCapability(cancelCtx, requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, validPayload(t)); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

// TestHostedCampusEnforcesCallDepth 用受限调用深度装配验证深度治理：
// 已达深度上限的调用被拒绝，未达上限的调用放行。
func TestHostedCampusEnforcesCallDepth(t *testing.T) {
	store := memory.NewBusStore()
	store.Replace(campus.AppID, []bus.Journey{})
	store.SetSnapshotMetadata(campus.AppID, authoritativeFreshMetadata())
	dispatcher := newHostedDispatcherWithDepth(t, store, 2, campus.AppID)
	// 深度 1（未达上限 2）允许进入。
	within := requestContext(campus.AppID)
	within.CallDepth = 1
	if _, err := dispatcher.InvokeCapability(context.Background(), within, campus.BusJourneySearchCapabilityID, validPayload(t)); err != nil {
		t.Fatalf("depth within limit error = %v, want nil", err)
	}
	// 深度 2（已达上限）被拒绝。
	atLimit := requestContext(campus.AppID)
	atLimit.CallDepth = 2
	if _, err := dispatcher.InvokeCapability(context.Background(), atLimit, campus.BusJourneySearchCapabilityID, validPayload(t)); !errors.Is(err, runtime.ErrCallDepthExceeded) {
		t.Fatalf("error = %v, want ErrCallDepthExceeded", err)
	}
}

// newHostedDispatcher 装配完整 hosted 链路（默认调用深度）。
func newHostedDispatcher(t *testing.T, store bus.Store, enabledApps ...string) *runtime.Dispatcher {
	return newHostedDispatcherWithDepth(t, store, 0, enabledApps...)
}

// newHostedDispatcherWithDepth 装配完整 hosted 链路：内置工件 → WasmHost →
// Loader → RegisterInstalled → Dispatcher；maxCallDepth 为 0 时使用 Dispatcher 默认值。
func newHostedDispatcherWithDepth(t *testing.T, store bus.Store, maxCallDepth uint16, enabledApps ...string) *runtime.Dispatcher {
	t.Helper()
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  campus.ReadArtifact,
		HostFunctions: campus.HostedFunctions(store),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("loader shutdown: %v", err)
		}
	})
	reg := registry.New()
	record := loader.InstalledRecord{
		Runtime:      campus.Manifest(),
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
	}
	if err := loader.RegisterInstalled(manager, reg, []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	// 预热 pin 的内置包：编译在装配时完成，保证行为断言不受首调编译影响。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Warmup(warmupCtx, []string{campus.ServiceID}, 1); err != nil {
		t.Fatalf("warm campus hosted package: %v", err)
	}
	policy := runtime.NewStaticAppPolicy()
	for _, appID := range enabledApps {
		policy.Enable(appID, campus.BusStopSearchCapabilityID)
		policy.Enable(appID, campus.BusRouteListCapabilityID)
		policy.Enable(appID, campus.BusJourneySearchCapabilityID)
	}
	options := []runtime.DispatcherOption{}
	if maxCallDepth > 0 {
		options = append(options, runtime.WithMaxCallDepth(maxCallDepth))
	}
	return runtime.NewDispatcher(reg, policy, options...)
}

// authoritativeFreshMetadata 返回权威新鲜的快照元数据，用于不受治理干扰的行为断言。
func authoritativeFreshMetadata() bus.SnapshotMetadata {
	now := time.Now().UTC()
	return bus.SnapshotMetadata{
		Revision: "revision-1", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
}

func requestContext(appID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID:           appID,
		EchoID:          "echo-test",
		RequestID:       "request-test",
		TraceID:         "trace-test",
		Deadline:        time.Now().Add(time.Minute),
		ProtocolVersion: "1.0",
	}
}

func validPayload(t *testing.T) json.RawMessage {
	t.Helper()
	return mustJSON(t, bus.SearchRequest{
		OriginStopID:      "stop-a",
		DestinationStopID: "stop-b",
		DepartAfter:       time.Now(),
	})
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

func journey(id, origin, destination string, departure time.Time) bus.Journey {
	return bus.Journey{
		TripID:            id,
		RouteID:           "route-1",
		RouteName:         "文理学部-信息学部",
		Direction:         "outbound",
		OriginStopID:      origin,
		OriginStopName:    origin,
		DestinationStopID: destination,
		DestinationName:   destination,
		DepartureAt:       departure,
		ArrivalAt:         departure.Add(20 * time.Minute),
		SourceRevision:    "test-revision",
	}
}
