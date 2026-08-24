//go:build !unix

// Package campustest 提供校园服务 hosted 装配的共享测试辅助。
// 非 Unix 平台：安装目录属主校验 fail-closed（无法等价验证文件属主，见
// loader/install_owner_other.go），catalog.Discover 不可用。这里内存构造
// InstalledRecord 并直接装配 WasmHost——wasm 执行与宿主函数投影跨平台可用，
// 仅"部署属主发现"是 Unix 边界（该边界由 loader 的 unix 测试覆盖）。
package campustest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/builtin"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
)

// RegisterHosted 以 hosted 包形态装配校园服务：内置 wasm 工件经进程内沙箱执行，
// 权威存储经宿主函数投影。非 Unix 平台不走安装目录发现（属主校验 fail-closed），
// 直接构造安装清单；wasm 执行与宿主函数投影链路与 Unix 一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store bus.Store) {
	t.Helper()
	digest := sha256.Sum256(builtin.CampusWASM)
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != campus.ServiceID || manifest.LockedDigest != hex.EncodeToString(digest[:]) {
				return nil, loader.ErrDescribeMismatch
			}
			return builtin.CampusWASM, nil
		},
		HostFunctions:        campus.HostedFunctions(store),
		RequireHostFunctions: true,
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	record := loader.InstalledRecord{
		Directory: "", ArtifactPath: "",
		Runtime: loader.Manifest{
			ID: campus.ServiceID, Version: "1.0.0", Mode: loader.ModeHosted,
			Role: loader.RoleCapability, LockedDigest: hex.EncodeToString(digest[:]),
			Pin: true,
			HostFunctions: []packmgr.HostedFunctionDecl{{
				Module: "ailuo.bus", Name: "query",
				Purpose: "查询 Go 托管权威校巴存储（App 隔离在宿主侧强制）",
			}},
		},
		PackageID: campus.ServiceID, ComponentID: "bus",
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
		Storage: &packmgr.Storage{
			Namespace:     "campus/bus",
			SchemaVersion: 1,
			Sensitivity:   packmgr.SensitivityPublic,
			Retention:     packmgr.RetentionPermanent,
		},
	}
	registerHosted(t, target, host, []loader.InstalledRecord{record})
}
