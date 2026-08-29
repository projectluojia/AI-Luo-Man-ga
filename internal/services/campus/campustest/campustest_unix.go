//go:build unix

// Package campustest 提供校园服务 hosted 装配的共享测试辅助，
// 供 campus、echo、e2e 等包以真实 hosted 链路验证能力。装配经真实安装目录
// 路径：构造 campus.bus 包目录（manifest + lock + 工件）→ catalog.Discover →
// 进程内 WasmHost（catalog.ReadArtifact + 宿主函数投影）→ RegisterInstalled，
// 与生产装配一致（campus 不再是内置包）。
//
// 安装目录属主校验仅 Unix 支持（非 Unix 平台 fail-closed，见
// packagesource/owner_other.go）：非 Unix 平台用 campustest_other.go 的内存
// 构造版本（wasm 执行与宿主函数投影跨平台可用，部署属主校验是 Unix 边界）。
package campustest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/builtin"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

// RegisterHosted 以真实安装目录路径装配校园服务：构造 campus.bus 安装包 →
// 进程内 WasmHost（宿主函数投影权威存储）→ 注册并预热。campus 是单组件包，
// 组件 ID 为 campus.BusComponentID，全部 Capability 由其导出。
func RegisterHosted(t testing.TB, target *registry.Registry, store bus.Store) {
	t.Helper()
	root := t.TempDir()
	writeCampusBusPackage(t, root)
	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	records, err := catalog.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(records) != 1 || records[0].ComponentID != campus.BusComponentID {
		t.Fatalf("Discover records = %+v, want single campus component", records)
	}
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:         catalog.ReadArtifact,
		HostFunctions:        campus.HostedFunctions(store),
		RequireHostFunctions: true,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	registerHosted(t, target, host, records)
}

// writeCampusBusPackage 构造 campus.bus 安装目录（manifest + lock + wasm 工件），
// 与生产安装形态一致：包目录名 = 包 ID，清单声明 host_functions/storage/
// extensions，工件 digest 锁定。
func writeCampusBusPackage(t testing.TB, root string) {
	t.Helper()
	directory := filepath.Join(root, campus.ServiceID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	extensions, err := campus.Extensions()
	if err != nil {
		t.Fatal(err)
	}
	manifest := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: campus.ServiceID, Version: "1.0.0",
		Storage: &packmgr.Storage{
			Namespace:     "campus/bus",
			SchemaVersion: 1,
			Sensitivity:   packmgr.SensitivityPublic,
			Retention:     packmgr.RetentionPermanent,
		},
		Extensions: extensions,
		Components: []packmgr.Component{{
			ID: campus.BusComponentID, Mode: loader.ModeHosted, Entrypoint: "campus.wasm",
			Exports: []string{
				campus.BusStopSearchCapabilityID,
				campus.BusRouteListCapabilityID,
				campus.BusJourneySearchCapabilityID,
			},
			HostFunctions: []packmgr.HostedFunctionDecl{{
				Module: "ailuo.bus", Name: "query",
				Purpose: "查询 Go 托管权威校巴存储（App 隔离在宿主侧强制）",
			}},
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifestBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(directory, "campus.wasm")
	if err := os.WriteFile(artifactPath, builtin.CampusWASM, 0o640); err != nil {
		t.Fatal(err)
	}
	artifactDigest := sha256.Sum256(builtin.CampusWASM)
	manifestDigest := sha256.Sum256(manifestBytes)
	lock, err := json.Marshal(packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: campus.ServiceID,
		PackageVersion: "1.0.0", ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packmgr.LockedArtifact{{
			ComponentID: campus.BusComponentID, Path: artifactPath, SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lock, 0o640); err != nil {
		t.Fatal(err)
	}
}
