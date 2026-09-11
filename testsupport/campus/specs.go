package campus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
)

// 校巴测试夹具使用的 App、包和组件标识。业务实现位于 packages/campus-bus；
// 本目录只供测试装配与 SDK 生成测试使用。
const (
	AppID            = "campus-services"
	PackageID        = "campus"
	BusComponentID   = "bus"
	StorageNamespace = "campus/bus"
)

// CapabilityID 是校巴能力标识（与 packages/campus-bus/ailuo.toml 的 exports
// 一致，漂移由 campustest 清单解析测试兜底）。
const (
	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
	BusRealtimeCapabilityID      = "campus.bus.vehicles.realtime"
)

// BusRoot 返回仓库内 campus-bus 包目录（testsupport/campus → 上溯两级到仓库
// 根，再进 packages/campus-bus）。
func BusRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate test source file")
	}
	// thisFile = <repo>/testsupport/campus/specs.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	root := filepath.Join(repoRoot, "packages", "campus-bus")
	if info, err := os.Stat(filepath.Join(root, "ailuo.toml")); err != nil || info.IsDir() {
		return "", fmt.Errorf("campus-bus package root not found near %s", thisFile)
	}
	return root, nil
}

var (
	manifestOnce sync.Once
	parsed       packagecontract.Manifest
	manifestErr  error
)

// ParsedManifest 解析真实的 packages/campus-bus/ailuo.toml（进程内只解析一次）：
// 能力规格与存储声明全部来自作者侧清单，测试与生产使用同一来源。
func ParsedManifest() (packagecontract.Manifest, error) {
	manifestOnce.Do(func() {
		root, err := BusRoot()
		if err != nil {
			manifestErr = err
			return
		}
		parsed, _, _, manifestErr = packagefmt.Parse(filepath.Join(root, "ailuo.toml"))
	})
	return parsed, manifestErr
}

// Capabilities 返回校巴对外暴露的 Capability 规格（来自真实 ailuo.toml）。
func Capabilities() ([]capability.CapabilitySpec, error) {
	manifest, err := ParsedManifest()
	if err != nil {
		return nil, err
	}
	return manifest.Capabilities, nil
}

// CapabilitiesJSON 返回供 SDK 生成测试使用的 Capability JSON。
func CapabilitiesJSON() (json.RawMessage, error) {
	capabilities, err := Capabilities()
	if err != nil {
		return nil, err
	}
	return json.Marshal(capabilities)
}
