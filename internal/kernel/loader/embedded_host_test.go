package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// embeddedBuiltin 构造一个内置包；invoke 为空时使用默认执行体（回显 AppID 与载荷大小）。
func embeddedBuiltin(id, version string, invoke func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)) loader.Builtin {
	if invoke == nil {
		invoke = func(_ context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"app_id": request.AppID, "bytes": len(payload)})
		}
	}
	return loader.Builtin{
		Manifest: loader.Manifest{ID: id, Version: version, Mode: loader.ModeEmbedded, LockedDigest: digest},
		Invoke:   invoke,
	}
}

func newEmbeddedManager(t *testing.T, builtins ...loader.Builtin) *loader.Manager {
	t.Helper()
	host, err := loader.NewEmbeddedHost(builtins)
	if err != nil {
		t.Fatalf("NewEmbeddedHost: %v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeEmbedded: host})
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	return manager
}

func TestEmbeddedHostRejectsInvalidBuiltins(t *testing.T) {
	valid := embeddedBuiltin("embedded.test", "1.0.0", nil)
	cases := []struct {
		name     string
		builtins []loader.Builtin
		want     error
	}{
		{name: "nil invoke", builtins: []loader.Builtin{{
			Manifest: loader.Manifest{ID: "a.test", Version: "1.0.0", Mode: loader.ModeEmbedded, LockedDigest: digest},
		}}, want: loader.ErrUnavailable},
		{name: "wrong mode", builtins: []loader.Builtin{{
			Manifest: loader.Manifest{ID: "a.test", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest},
			Invoke: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
				return nil, nil
			},
		}}, want: loader.ErrUnsupportedMode},
		{name: "bad digest", builtins: []loader.Builtin{{
			Manifest: loader.Manifest{ID: "a.test", Version: "1.0.0", Mode: loader.ModeEmbedded, LockedDigest: "short"},
			Invoke: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
				return nil, nil
			},
		}}, want: loader.ErrInvalidManifest},
		{name: "bad id", builtins: []loader.Builtin{{
			Manifest: loader.Manifest{ID: "UPPER.test", Version: "1.0.0", Mode: loader.ModeEmbedded, LockedDigest: digest},
			Invoke: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
				return nil, nil
			},
		}}, want: loader.ErrInvalidManifest},
		{name: "duplicate id", builtins: []loader.Builtin{valid, valid}, want: loader.ErrDuplicateID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loader.NewEmbeddedHost(tc.builtins)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewEmbeddedHost error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmbeddedHostVerifyMatchesLockedManifest(t *testing.T) {
	host, err := loader.NewEmbeddedHost([]loader.Builtin{embeddedBuiltin("embedded.test", "1.0.0", nil)})
	if err != nil {
		t.Fatalf("NewEmbeddedHost: %v", err)
	}
	base := loader.Manifest{ID: "embedded.test", Version: "1.0.0", Mode: loader.ModeEmbedded, LockedDigest: digest}
	if err := host.Verify(context.Background(), base); err != nil {
		t.Fatalf("Verify(valid) = %v, want nil", err)
	}
	cases := []struct {
		name string
		edit func(*loader.Manifest)
		want error
	}{
		{name: "unknown id", edit: func(m *loader.Manifest) { m.ID = "missing.test" }, want: loader.ErrNotFound},
		{name: "version mismatch", edit: func(m *loader.Manifest) { m.Version = "2.0.0" }, want: loader.ErrDescribeMismatch},
		{name: "digest mismatch", edit: func(m *loader.Manifest) {
			m.LockedDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abceef"
		}, want: loader.ErrDescribeMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := base
			tc.edit(&manifest)
			if err := host.Verify(context.Background(), manifest); !errors.Is(err, tc.want) {
				t.Fatalf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmbeddedRuntimeLoadsOnceAndServesInProcess(t *testing.T) {
	var invokes atomic.Int32
	var seenAppID atomic.Value
	builtin := embeddedBuiltin("embedded.test", "1.0.0", func(_ context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		invokes.Add(1)
		seenAppID.Store(request.AppID)
		return json.Marshal(map[string]any{"payload_bytes": len(payload)})
	})
	manager := newEmbeddedManager(t, builtin)
	ctx := context.Background()
	if err := manager.Register(builtin.Manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 首次加载后常驻：连续两次 Acquire 都直接命中进程内 Runtime。
	for attempt := 0; attempt < 2; attempt++ {
		lease, err := manager.Acquire(ctx, "embedded.test")
		if err != nil {
			t.Fatalf("Acquire #%d: %v", attempt, err)
		}
		payload := json.RawMessage(`{"query":"bus"}`)
		result, err := lease.Invoke(ctx, contracts.RequestContext{AppID: "app.campus"}, payload)
		if err != nil {
			t.Fatalf("Invoke #%d: %v", attempt, err)
		}
		var decoded struct {
			PayloadBytes int `json:"payload_bytes"`
		}
		if err := json.Unmarshal(result, &decoded); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if decoded.PayloadBytes != len(payload) {
			t.Fatalf("Invoke #%d payload bytes = %d, want %d", attempt, decoded.PayloadBytes, len(payload))
		}
		lease.Release()
	}
	if invokes.Load() != 2 {
		t.Fatalf("invokes = %d, want 2", invokes.Load())
	}
	if appID, _ := seenAppID.Load().(string); appID != "app.campus" {
		t.Fatalf("governed context app_id = %q, want app.campus", appID)
	}
	snapshot, err := manager.Snapshot("embedded.test")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.State != loader.StateReady || snapshot.Mode != loader.ModeEmbedded {
		t.Fatalf("snapshot = %+v, want ready embedded", snapshot)
	}

	// 停机：常驻内置包同样按 Loader 生命周期排空并清理。
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := manager.Acquire(context.Background(), "embedded.test"); !errors.Is(err, loader.ErrShuttingDown) {
		t.Fatalf("Acquire after shutdown = %v, want ErrShuttingDown", err)
	}
}

func TestEmbeddedVerifyFailsFastBeforeExecution(t *testing.T) {
	manager := newEmbeddedManager(t, embeddedBuiltin("embedded.test", "1.0.0", nil))
	// 注册的 manifest 与内置包版本不一致：Verify 失败，EnsureLoaded 快速失败，不执行任何代码。
	mismatched := embeddedBuiltin("embedded.test", "2.0.0", nil).Manifest
	if err := manager.Register(mismatched); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := manager.EnsureLoaded(context.Background(), "embedded.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("EnsureLoaded error = %v, want ErrLoadFailed", err)
	}
	if _, err := manager.Acquire(context.Background(), "embedded.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("Acquire error = %v, want ErrLoadFailed", err)
	}
}

func TestEmbeddedPinnedUnloadRejectedAndShutdownForcesCleanup(t *testing.T) {
	builtin := embeddedBuiltin("embedded.pin", "1.0.0", nil)
	builtin.Manifest.Pin = true
	manager := newEmbeddedManager(t, builtin)
	ctx := context.Background()
	if err := manager.Register(builtin.Manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := manager.EnsureLoaded(ctx, "embedded.pin"); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if err := manager.Unload(ctx, "embedded.pin"); !errors.Is(err, loader.ErrPinned) {
		t.Fatalf("Unload error = %v, want ErrPinned", err)
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestEmbeddedRejectsUnsupportedUpgradeManifest(t *testing.T) {
	// 内置包升级 = 内核重新发布（代码编译进内核）；运行期传入新版本 manifest 必须被拒绝，
	// 与"锁定的 manifest 必须精确匹配内置包"的规则一致。
	manager := newEmbeddedManager(t, embeddedBuiltin("embedded.upgrade", "1.0.0", nil))
	ctx := context.Background()
	if err := manager.Register(embeddedBuiltin("embedded.upgrade", "1.0.0", nil).Manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	next := embeddedBuiltin("embedded.upgrade", "2.0.0", nil).Manifest
	if err := manager.Upgrade(ctx, next); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("Upgrade error = %v, want ErrLoadFailed", err)
	}
	snapshot, err := manager.Snapshot("embedded.upgrade")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Version != "1.0.0" {
		t.Fatalf("version after rejected upgrade = %s, want 1.0.0", snapshot.Version)
	}
}
