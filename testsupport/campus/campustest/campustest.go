// Package campustest 提供校园 App hosted 装配的共享测试辅助。
//
// 与 timetabletest 同模式：不做 guest 源码复制，直接读取仓库内真实
// packages/campus-bus 清单（ailuo.toml）与源码（src/、bus/、guestkit），
// 由 packagefmt go-wasm 构建器现场编译——清单与 guest 的测试即生产形态。
// 校巴数据由测试经 packstore.Store 播种（App 隔离由 Scope 强制），guest 经
// ailuo.store 宿主函数读取，完整链路与生产安装包一致。
package campustest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus"
)

// compiledGuest 是现场编译的 guest 工件（进程内只编译一次）。
var (
	buildOnce     sync.Once
	artifactBytes []byte
	buildErr      error
)

// RegisterHosted 以 hosted 包形态装配 campus-bus 包：真实清单 + 真实 guest
// 源码，ailuo.store 宿主函数绑定到 packstore 端口（App 隔离由 Scope 强制），
// 与生产安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	artifact := buildGuest(t)
	digest := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(digest[:])

	root, err := campus.BusRoot()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		t.Fatalf("parse campus-bus ailuo.toml: %v", err)
	}
	if manifest.ID != campus.PackageID || manifest.Storage == nil || manifest.Storage.Namespace != campus.StorageNamespace {
		t.Fatalf("unexpected manifest id=%q storage=%#v", manifest.ID, manifest.Storage)
	}
	loaderManifest := loader.Manifest{
		ID: manifest.ID, PackageID: manifest.ID, Version: manifest.Version, Mode: loader.ModeHosted,
		Role: loader.RoleProvider, LockedDigest: artifactDigest, Pin: true,
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
		PackageID: manifest.ID, ComponentID: campus.BusComponentID,
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
		t.Fatalf("warm campus hosted package: %v", err)
	}
}

// buildGuest 现场编译真实 packages/campus-bus 源码（整个测试进程只编译一次）。
func buildGuest(t testing.TB) []byte {
	t.Helper()
	buildOnce.Do(func() {
		artifactBytes, buildErr = compileGuest()
	})
	if buildErr != nil {
		t.Fatalf("build campus-bus guest wasm: %v", buildErr)
	}
	return artifactBytes
}

func compileGuest() ([]byte, error) {
	root, err := campus.BusRoot()
	if err != nil {
		return nil, err
	}
	manifest, _, builds, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		return nil, fmt.Errorf("parse campus-bus ailuo.toml: %w", err)
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
