package packmgr

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
)

// Package 是分发、版本与升级单位；Component 是运行单元，每个组件恰好一种
// 运行模式（hosted/isolated）。包级清单声明组件列表与包级扩展段；组件间
// 通过统一 Capability（imports/exports）通信，依赖拓扑决定启动与停止顺序。
// 宿主扩展段（Extensions）由宿主解释（AI珞 内核期望 tools/service/capabilities）。

// Manifest 是中立的包清单。
type Manifest struct {
	SchemaVersion string       `json:"schema_version"`
	ID            string       `json:"id"`
	Version       string       `json:"version"`
	Pin           bool         `json:"pin,omitempty"`
	IdleTTLMS     uint64       `json:"idle_ttl_ms,omitempty"`
	Components    []Component  `json:"components"`
	Storage       *Storage     `json:"storage,omitempty"`
	Dependencies  []Dependency `json:"dependencies,omitempty"`
	// Extensions 是宿主扩展段（json.RawMessage），原样保留给宿主解释。
	Extensions json.RawMessage `json:"extensions,omitempty"`
}

// Component 是一个运行单元：恰好一种 mode、一个 entrypoint 工件，export 提供
// 的 Capability、import 消费的 Capability。HostFunctions 是 wasm 沙箱权利
// （第二权限轴），isolated 组件的进程规格由 lock 固化。
type Component struct {
	ID            string               `json:"id"`
	Mode          string               `json:"mode"`
	Entrypoint    string               `json:"entrypoint"`
	Exports       []string             `json:"exports,omitempty"`
	Imports       []string             `json:"imports,omitempty"`
	HostFunctions []HostedFunctionDecl `json:"host_functions,omitempty"`
}

// Lock 是安装目录的锁定记录：固定包版本、清单摘要与每组件工件/进程规格。
type Lock struct {
	SchemaVersion  string           `json:"schema_version"`
	PackageID      string           `json:"package_id"`
	PackageVersion string           `json:"package_version"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Artifacts      []LockedArtifact `json:"artifacts"`
}

// LockedArtifact 是单个组件的锁定工件：路径、SHA-256 与（isolated 的）进程规格。
type LockedArtifact struct {
	ComponentID string       `json:"component_id"`
	Path        string       `json:"path"`
	SHA256      string       `json:"sha256"`
	Process     *ProcessSpec `json:"process,omitempty"`
}

// ProcessSpec 是进程执行规格（isolated 模式）；Limits 为通用资源上限。
type ProcessSpec struct {
	Path    string        `json:"path"`
	Args    []string      `json:"args"`
	Env     []string      `json:"env"`
	WorkDir string        `json:"work_dir"`
	Address string        `json:"address"`
	Limits  ProcessLimits `json:"limits,omitempty"`
}

// ProcessLimits 是通用进程资源上限（各平台按能力应用；不支持的平台 fail closed）。
type ProcessLimits struct {
	MaxAddressBytes uint64 `json:"max_address_bytes,omitempty"`
	MaxCPUSeconds   uint64 `json:"max_cpu_seconds,omitempty"`
	MaxOpenFiles    uint64 `json:"max_open_files,omitempty"`
	MaxFileBytes    uint64 `json:"max_file_bytes,omitempty"`
}

// InstalledRecord 是中立的安装目录记录（清单 + lock）。
type InstalledRecord struct {
	Directory string
	Manifest  Manifest
	Lock      Lock
}

// ValidateManifest 校验包清单：组件唯一性、mode/entrypoint 闭合、imports/exports
// 标识合法、依赖拓扑无环。宿主扩展段在宿主解析时严格解码。
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || !stableLowerPattern.MatchString(manifest.ID) ||
		len(manifest.ID) > 128 {
		return ErrInvalidFormat
	}
	if _, err := ParseVersion(manifest.Version); err != nil {
		return ErrInvalidFormat
	}
	if manifest.IdleTTLMS > 2592000000 { // 30 天
		return ErrInvalidFormat
	}
	if len(manifest.Components) == 0 {
		return ErrInvalidFormat
	}
	if manifest.Storage != nil {
		if err := ValidateStorage(*manifest.Storage); err != nil {
			return ErrInvalidFormat
		}
	}
	for _, dep := range manifest.Dependencies {
		if err := ValidateDependency(dep); err != nil {
			return ErrInvalidFormat
		}
	}
	if len(manifest.Extensions) > 0 && !json.Valid(manifest.Extensions) {
		return ErrInvalidFormat
	}
	seenComponents := make(map[string]struct{}, len(manifest.Components))
	for _, component := range manifest.Components {
		if !stableLowerPattern.MatchString(component.ID) || len(component.ID) > 128 {
			return ErrInvalidFormat
		}
		if _, duplicate := seenComponents[component.ID]; duplicate {
			return ErrInvalidFormat
		}
		seenComponents[component.ID] = struct{}{}
		if component.Mode != ModeHosted && component.Mode != ModeIsolated {
			return ErrInvalidFormat
		}
		if component.Entrypoint == "" {
			return ErrInvalidFormat
		}
		for _, id := range append(append([]string(nil), component.Exports...), component.Imports...) {
			if !stableLowerPattern.MatchString(id) || len(id) > 128 {
				return ErrInvalidFormat
			}
		}
		if err := ValidateHostedFunctions(component.HostFunctions); err != nil {
			return ErrInvalidFormat
		}
	}
	if _, err := ComponentOrder(manifest.Components); err != nil {
		return err
	}
	return nil
}

// ComponentOrder 按依赖拓扑对组件排序（Kahn 算法）：组件 imports 的 Capability
// 若由同包另一组件 exports，则提供方在前。未在同包解析的 import 不构成边
// （由包依赖/宿主注册表解析）。有环返回错误。
func ComponentOrder(components []Component) ([]string, error) {
	exporterByCapability := make(map[string]string, len(components))
	for _, component := range components {
		for _, capability := range component.Exports {
			if _, duplicate := exporterByCapability[capability]; duplicate {
				return nil, fmt.Errorf("%w: Capability %q 被多个组件导出", ErrInvalidFormat, capability)
			}
			exporterByCapability[capability] = component.ID
		}
	}
	indexByID := make(map[string]int, len(components))
	for index, component := range components {
		indexByID[component.ID] = index
	}
	dependents := make([][]int, len(components)) // 反向边：provider → consumers
	indegree := make([]int, len(components))
	for index, component := range components {
		for _, capability := range component.Imports {
			provider, ok := exporterByCapability[capability]
			if !ok {
				continue // 由包依赖/注册表提供，不构成包内边
			}
			providerIndex, ok := indexByID[provider]
			if !ok {
				return nil, fmt.Errorf("%w: import %q 引用未知组件 %q", ErrInvalidFormat, capability, provider)
			}
			dependents[providerIndex] = append(dependents[providerIndex], index)
			indegree[index]++
		}
	}
	order := make([]string, 0, len(components))
	queue := make([]int, 0)
	for index, degree := range indegree {
		if degree == 0 {
			queue = append(queue, index)
		}
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		order = append(order, components[current].ID)
		for _, dependent := range dependents[current] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	if len(order) != len(components) {
		return nil, fmt.Errorf("%w: 组件依赖存在环", ErrInvalidFormat)
	}
	return order, nil
}

// ValidateLock 校验锁定记录：包身份一致、清单摘要长度、每组件工件路径/摘要/
// 进程规格闭合。
func ValidateLock(lock Lock, manifest Manifest) error {
	if lock.SchemaVersion != SchemaVersion || lock.PackageID != manifest.ID ||
		lock.PackageVersion != manifest.Version || !stableLowerPattern.MatchString(lock.PackageID) ||
		len(lock.PackageID) > 128 {
		return ErrInvalidFormat
	}
	if _, err := ParseVersion(lock.PackageVersion); err != nil {
		return ErrInvalidFormat
	}
	if len(lock.ManifestSHA256) != 64 {
		return ErrInvalidFormat
	}
	if len(lock.Artifacts) != len(manifest.Components) {
		return ErrInvalidFormat
	}
	seen := make(map[string]struct{}, len(lock.Artifacts))
	for _, artifact := range lock.Artifacts {
		component, ok := findComponent(manifest, artifact.ComponentID)
		if !ok {
			return ErrInvalidFormat
		}
		if _, duplicate := seen[artifact.ComponentID]; duplicate {
			return ErrInvalidFormat
		}
		seen[artifact.ComponentID] = struct{}{}
		if len(artifact.SHA256) != 64 {
			return ErrInvalidFormat
		}
		if !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path {
			return ErrInvalidFormat
		}
		switch component.Mode {
		case ModeHosted:
			if artifact.Process != nil {
				return ErrInvalidFormat
			}
		case ModeIsolated:
			if artifact.Process == nil || artifact.Process.Path != artifact.Path {
				return ErrInvalidFormat
			}
			if err := ValidateProcessSpec(*artifact.Process); err != nil {
				return ErrInvalidFormat
			}
		default:
			return ErrInvalidFormat
		}
	}
	return nil
}

func findComponent(manifest Manifest, id string) (Component, bool) {
	for _, component := range manifest.Components {
		if component.ID == id {
			return component, true
		}
	}
	return Component{}, false
}

// ValidateProcessSpec 校验进程执行规格的形状：绝对路径、本地地址、参数与
// 环境数量上限、资源上限闭式。文件系统存在性校验由宿主在装载时执行。
func ValidateProcessSpec(spec ProcessSpec) error {
	if !filepath.IsAbs(spec.Path) || filepath.Clean(spec.Path) != spec.Path ||
		!filepath.IsAbs(spec.WorkDir) || filepath.Clean(spec.WorkDir) != spec.WorkDir ||
		!IsLocalRuntimeAddress(spec.Address) || len(spec.Args) > 128 || len(spec.Env) > 64 ||
		!ValidProcessLimits(spec.Limits) {
		return ErrInvalidFormat
	}
	return nil
}

// IsLocalRuntimeAddress 只接受本机地址：loopback 或绝对 Unix socket。
func IsLocalRuntimeAddress(address string) bool {
	if strings.HasPrefix(address, "unix:") {
		socketPath := strings.TrimPrefix(address, "unix:")
		return filepath.IsAbs(socketPath) && filepath.Clean(socketPath) == socketPath
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidProcessLimits 校验资源上限不超过闭式上限。
func ValidProcessLimits(limits ProcessLimits) bool {
	return limits.MaxAddressBytes <= maxProcessLimitAddress &&
		limits.MaxCPUSeconds <= maxProcessLimitCPU &&
		limits.MaxOpenFiles <= maxProcessLimitFiles &&
		limits.MaxFileBytes <= maxProcessLimitFile
}

const (
	maxProcessLimitAddress = uint64(1 << 40) // 1 TiB
	maxProcessLimitCPU     = uint64(1 << 31)
	maxProcessLimitFiles   = uint64(1 << 20)
	maxProcessLimitFile    = uint64(1 << 40)
)
