// Package campustest 提供校园 App hosted 装配的共享测试辅助。
//
// guest 源码是本包 testdata 下的真实 Go 文件（testdata/guest/main.go），读入后
// 由 packagefmt go-wasm 构建器在测试时现场编译——仓库不保存
// 任何测试用 wasm 包工件。guest 实现校园三种能力（站点/线路/行程）并通过通用
// ailuo.store 宿主函数读取宿主存储，与生产形态一致：业务逻辑在 guest，状态
// 经 packstore 端口留在宿主。数据由测试经 packstore.Store 播种，App 隔离在
// 宿主侧强制。
package campustest

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus"
)

//go:embed testdata/guest/main.go
var guestSource []byte

const guestEntrypointName = "campus.wasm"

var (
	guestBuildOnce sync.Once
	guestArtifact  []byte
	guestBuildErr  error
)

// RegisterHosted 以 hosted 包形态装配 campus.bus.routes.list：测试播种数据经
// packstore 端口写入（App 隔离由 Scope 强制），guest 经 ailuo.store 宿主函数
// 读取，完整链路与生产安装包一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	artifact := buildGuest(t)
	digest := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(digest[:])

	storage := &packagecontract.Storage{
		Namespace: campus.StorageNamespace,
	}
	manifest := loader.Manifest{
		ID: campus.PackageID, PackageID: campus.PackageID, Version: campus.PackageVersion, Mode: loader.ModeHosted,
		Role: loader.RoleProvider, LockedDigest: artifactDigest, Pin: true,
		Storage:      storage,
		Capabilities: campus.Capabilities(),
		HostFunctions: []packagecontract.HostedFunctionDecl{
			{Module: packstore.StoreModule, Name: packstore.OpList, Purpose: "列出包命名空间内的集合文档并携带快照元数据"},
		},
	}
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, m loader.Manifest) ([]byte, error) {
			if m.ID != manifest.ID || m.LockedDigest != manifest.LockedDigest {
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
		Runtime:   manifest,
		PackageID: campus.PackageID, ComponentID: campus.BusComponentID,
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
	warmupContext, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Warmup(warmupContext, []string{manifest.ID}, 1); err != nil {
		t.Fatalf("warm campus hosted package: %v", err)
	}
}

// buildGuest 现场编译 guest 源码（整个测试进程只编译一次）。
func buildGuest(t testing.TB) []byte {
	t.Helper()
	guestBuildOnce.Do(func() {
		guestArtifact, guestBuildErr = compileGuest()
	})
	if guestBuildErr != nil {
		t.Fatalf("build campus test guest: %v", guestBuildErr)
	}
	return guestArtifact
}

func compileGuest() ([]byte, error) {
	sourceDir, err := os.MkdirTemp("", "campustest-guest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(sourceDir)
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(guestSource), 0o640); err != nil {
		return nil, err
	}
	goMod := "module campus-test-guest\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte(goMod), 0o640); err != nil {
		return nil, err
	}
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: campus.PackageID, Version: campus.PackageVersion,
		Storage: &packagecontract.Storage{
			Namespace: campus.StorageNamespace,
		},
		Components: []packagecontract.Component{{
			ID: campus.BusComponentID, Mode: packagecontract.ModeHosted, Role: packagecontract.RoleProvider, Entrypoint: guestEntrypointName,
			Exports: []string{
				campus.BusStopSearchCapabilityID, campus.BusRouteListCapabilityID, campus.BusJourneySearchCapabilityID,
			},
		}},
		Capabilities: campus.Capabilities(),
	}
	if err := packagefmt.Build(context.Background(), sourceDir, manifest, []packagefmt.BuildSpec{{Tool: packagefmt.BuildToolGoWasm}}); err != nil {
		return nil, fmt.Errorf("build guest wasm: %w", err)
	}
	return os.ReadFile(filepath.Join(sourceDir, guestEntrypointName))
}
