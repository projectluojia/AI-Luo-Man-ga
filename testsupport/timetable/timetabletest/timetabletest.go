// Package timetabletest 提供课表 hosted 包装配的共享测试辅助。
//
// 与 campustest 不同，本包不做 guest 源码复制：直接读取仓库内真实
// packages/timetable 清单（ailuo.toml）与源码（src/、tt/、app/、guestkit），
// 由 packagefmt go-wasm 构建器现场编译——清单与 guest 的测试即生产形态。
// 课表数据全部落在个人作用域，测试调用方需携带已认证 UserID。
package timetabletest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
)

// PackageID / PackageVersion / StorageNamespace 与 packages/timetable/ailuo.toml
// 保持一致（清单解析测试会校验二者不漂移）。
const (
	PackageID        = "timetable"
	PackageVersion   = "1.0.0"
	StorageNamespace = "timetable/tables"
	ComponentID      = "provider"
)

// CapabilityID 是 12 个课表能力标识（与清单 exports 一致）。
const (
	CapabilityTimetableList     = "timetable.list"
	CapabilityTimetableGet      = "timetable.get"
	CapabilityTimetableCreate   = "timetable.create"
	CapabilityTimetableUpdate   = "timetable.update"
	CapabilityTimetableDelete   = "timetable.delete"
	CapabilityTimetableActivate = "timetable.activate"
	CapabilityCourseList        = "timetable.course.list"
	CapabilityCourseGet         = "timetable.course.get"
	CapabilityCourseCreate      = "timetable.course.create"
	CapabilityCourseUpdate      = "timetable.course.update"
	CapabilityCourseDelete      = "timetable.course.delete"
	CapabilityImport            = "timetable.import"
)

// CapabilityIDs 返回全部能力标识（测试批量启用策略用）。
func CapabilityIDs() []string {
	return []string{
		CapabilityTimetableList, CapabilityTimetableGet, CapabilityTimetableCreate,
		CapabilityTimetableUpdate, CapabilityTimetableDelete, CapabilityTimetableActivate,
		CapabilityCourseList, CapabilityCourseGet, CapabilityCourseCreate,
		CapabilityCourseUpdate, CapabilityCourseDelete, CapabilityImport,
	}
}

// TimetableRoot 返回仓库内课表包目录（testsupport/timetable/timetabletest →
// 上溯三级到仓库根，再进 packages/timetable）。
func TimetableRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate test source file")
	}
	// thisFile = <repo>/testsupport/timetable/timetabletest/timetabletest.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	root := filepath.Join(repoRoot, "packages", "timetable")
	if info, err := os.Stat(filepath.Join(root, "ailuo.toml")); err != nil || info.IsDir() {
		return "", fmt.Errorf("timetable package root not found near %s", thisFile)
	}
	return root, nil
}

// compiledGuest 是现场编译的 guest 工件（进程内只编译一次）。
var (
	buildOnce     sync.Once
	artifactBytes []byte
	buildErr      error
)

// RegisterHosted 以 hosted 包形态装配课表包：真实清单 + 真实 guest 源码，
// ailuo.store 宿主函数绑定到 packstore 端口（用户作用域由治理上下文注入
// UserID），与生产安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	artifact := buildGuest(t)
	digest := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(digest[:])

	root, err := TimetableRoot()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		t.Fatalf("parse timetable ailuo.toml: %v", err)
	}
	if manifest.ID != PackageID || manifest.Storage == nil || manifest.Storage.Namespace != StorageNamespace {
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
		PackageID: PackageID, ComponentID: ComponentID,
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
	if err := manager.Warmup(warmupContext, []string{loaderManifest.ID}, 1); err != nil {
		t.Fatalf("warm timetable hosted package: %v", err)
	}
}

// buildGuest 现场编译真实 packages/timetable 源码（整个测试进程只编译一次）。
func buildGuest(t testing.TB) []byte {
	t.Helper()
	buildOnce.Do(func() {
		artifactBytes, buildErr = compileGuest()
	})
	if buildErr != nil {
		t.Fatalf("build timetable guest wasm: %v", buildErr)
	}
	return artifactBytes
}

func compileGuest() ([]byte, error) {
	root, err := TimetableRoot()
	if err != nil {
		return nil, err
	}
	manifest, _, builds, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		return nil, fmt.Errorf("parse timetable ailuo.toml: %w", err)
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
