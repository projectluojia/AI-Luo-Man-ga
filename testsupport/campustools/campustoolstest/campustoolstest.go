// Package campustoolstest 提供空闲教室与学业校历 hosted 包装配的共享测试辅助。
//
// 与 timetabletest/campustest 同模式：不做 guest 源码复制，直接读取仓库内真实
// packages/classroom、packages/calendar 清单（ailuo.toml）与源码，由 packagefmt
// go-wasm 构建器现场编译——清单与 guest 的测试即生产形态。快照数据由测试经
// packstore.Store 播种（App 隔离由 Scope 强制），日程写路径由治理上下文注入
// UserID。
package campustoolstest

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

// packageSpec 描述一个被装配的 hosted 包：仓库内目录与测试夹具标识。
type packageSpec struct {
	dir              string
	componentID      string
	storageNamespace string
}

var (
	classroomSpec = packageSpec{dir: "classroom", componentID: "provider", storageNamespace: "classroom/rooms"}
	calendarSpec  = packageSpec{dir: "calendar", componentID: "provider", storageNamespace: "calendar/events"}
)

// Classroom 包与组件标识（与 packages/classroom/ailuo.toml 一致）。
const (
	ClassroomPackageID        = "classroom"
	ClassroomComponentID      = "provider"
	ClassroomStorageNamespace = "classroom/rooms"
)

// Classroom Capability 标识（与清单 exports 一致，漂移由清单解析测试兜底）。
const (
	ClassroomRoomsSearchCapabilityID    = "classroom.rooms.search"
	ClassroomCampusesListCapabilityID   = "classroom.campuses.list"
	ClassroomBuildingsListCapabilityID  = "classroom.buildings.list"
	ClassroomScheduleCreateCapabilityID = "classroom.schedule.create"
	ClassroomScheduleListCapabilityID   = "classroom.schedule.list"
	ClassroomScheduleCancelCapabilityID = "classroom.schedule.cancel"
)

// Calendar 包与能力标识（与 packages/calendar/ailuo.toml 一致）。
const (
	CalendarPackageID              = "calendar"
	CalendarComponentID            = "provider"
	CalendarStorageNamespace       = "calendar/events"
	CalendarEventsListCapabilityID = "calendar.events.list"
)

// ClassroomCapabilityIDs 返回教室全部能力标识（测试批量启用策略用）。
func ClassroomCapabilityIDs() []string {
	return []string{
		ClassroomRoomsSearchCapabilityID, ClassroomCampusesListCapabilityID,
		ClassroomBuildingsListCapabilityID, ClassroomScheduleCreateCapabilityID,
		ClassroomScheduleListCapabilityID, ClassroomScheduleCancelCapabilityID,
	}
}

func packageRoot(spec packageSpec) (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate test source file")
	}
	// thisFile = <repo>/testsupport/campustools/campustoolstest/campustoolstest.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	root := filepath.Join(repoRoot, "packages", spec.dir)
	if info, err := os.Stat(filepath.Join(root, "ailuo.toml")); err != nil || info.IsDir() {
		return "", fmt.Errorf("%s package root not found near %s", spec.dir, thisFile)
	}
	return root, nil
}

// compiledPackage 是现场编译的 guest 工件缓存（进程内每包只编译一次）。
type compiledPackage struct {
	once     sync.Once
	artifact []byte
	err      error
}

var (
	classroomCompiled compiledPackage
	calendarCompiled  compiledPackage
)

// RegisterClassroomHosted 以 hosted 包形态装配空闲教室包：真实清单 + 真实
// guest 源码，ailuo.store 宿主函数绑定到 packstore 端口，与生产安装包链路一致。
func RegisterClassroomHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	registerHosted(t, target, store, classroomSpec, &classroomCompiled)
}

// RegisterCalendarHosted 以 hosted 包形态装配学业校历包。
func RegisterCalendarHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	registerHosted(t, target, store, calendarSpec, &calendarCompiled)
}

func registerHosted(t testing.TB, target *registry.Registry, store packstore.Store, spec packageSpec, compiled *compiledPackage) {
	t.Helper()
	artifact := buildGuest(t, spec, compiled)
	digest := sha256.Sum256(artifact)
	artifactDigest := hex.EncodeToString(digest[:])

	root, err := packageRoot(spec)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		t.Fatalf("parse %s ailuo.toml: %v", spec.dir, err)
	}
	if manifest.Storage == nil || manifest.Storage.Namespace != spec.storageNamespace {
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
		PackageID: manifest.ID, ComponentID: spec.componentID,
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
		t.Fatalf("warm %s hosted package: %v", spec.dir, err)
	}
}

// buildGuest 现场编译真实包源码（整个测试进程每包只编译一次）。
func buildGuest(t testing.TB, spec packageSpec, compiled *compiledPackage) []byte {
	t.Helper()
	compiled.once.Do(func() {
		compiled.artifact, compiled.err = compileGuest(spec)
	})
	if compiled.err != nil {
		t.Fatalf("build %s guest wasm: %v", spec.dir, compiled.err)
	}
	return compiled.artifact
}

func compileGuest(spec packageSpec) ([]byte, error) {
	root, err := packageRoot(spec)
	if err != nil {
		return nil, err
	}
	manifest, _, builds, err := packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	if err != nil {
		return nil, fmt.Errorf("parse %s ailuo.toml: %w", spec.dir, err)
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
