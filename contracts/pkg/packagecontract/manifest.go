package packagecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
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
	ID string `json:"id"`
	// Mode 是运行形态：hosted（内核内 wasm 沙箱）或 isolated（独立进程）。
	Mode string `json:"mode"`
	// Role 是运行角色：capability（缺省，被调用的功能）或 executor（发起调用
	// 的认知运行时）。executor 必须 isolated——wasm 沙箱无出站，装不下思考者；
	// 部署策略要求 executor 包经过签名信任。
	Role       string `json:"role,omitempty"`
	Entrypoint string `json:"entrypoint"`
	// Process 是 isolated 组件必需的包内相对进程模板；安装时由 packmgr 解析为
	// lock 中的绝对 ProcessSpec。模板不包含凭据，Provider 等部署配置不属于
	// Core 包契约。
	Process       *ProcessTemplate     `json:"process,omitempty"`
	Exports       []string             `json:"exports,omitempty"`
	Imports       []string             `json:"imports,omitempty"`
	HostFunctions []HostedFunctionDecl `json:"host_functions,omitempty"`
}

// ProcessTemplate 是包作者声明的 isolated 进程启动模板。Path 与 WorkDir
// 相对组件工件目录，Address 只允许本机地址；WorkDir/Address 省略时由安装器
// 使用工件根目录和包内 Unix Socket 的确定性默认值，再转换为绝对路径。
type ProcessTemplate struct {
	Path    string   `json:"path"`
	Args    []string `json:"args,omitempty"`
	WorkDir string   `json:"work_dir,omitempty"`
	Address string   `json:"address,omitempty"`
}

// 组件运行角色的闭式取值。
const (
	RoleCapability = "capability"
	RoleExecutor   = "executor"
)

// Lock 是包发布归档或安装目录的锁定记录：固定包版本、清单摘要与每组件工件。
// 归档使用包根相对路径且不含 Process；安装目录使用绝对路径并为 isolated
// 组件固化完整进程规格，分别由 ValidateArchiveLock/ValidateLock 校验。
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
	Path string   `json:"path"`
	Args []string `json:"args"`
	// Env 是安装锁定的非秘密环境项；宿主不会从自身环境或部署配置补齐它。
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

// ValidateManifest 校验包清单：组件唯一性、mode/entrypoint 闭合、imports/exports
// 标识合法、依赖拓扑无环。宿主扩展段在宿主解析时严格解码。
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || !capability.IsStableID(manifest.ID) {
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
	seenDependencies := make(map[string]struct{}, len(manifest.Dependencies))
	for _, dep := range manifest.Dependencies {
		if err := ValidateDependency(dep); err != nil {
			return ErrInvalidFormat
		}
		if _, duplicate := seenDependencies[dep.ID]; duplicate {
			return ErrInvalidFormat
		}
		seenDependencies[dep.ID] = struct{}{}
	}
	if len(manifest.Extensions) > 0 && !json.Valid(manifest.Extensions) {
		return ErrInvalidFormat
	}
	seenComponents := make(map[string]struct{}, len(manifest.Components))
	for _, component := range manifest.Components {
		if !capability.IsStableID(component.ID) {
			return ErrInvalidFormat
		}
		if _, duplicate := seenComponents[component.ID]; duplicate {
			return ErrInvalidFormat
		}
		seenComponents[component.ID] = struct{}{}
		if component.Mode != ModeHosted && component.Mode != ModeIsolated {
			return ErrInvalidFormat
		}
		switch component.Role {
		case "", RoleCapability:
		case RoleExecutor:
			// wasm 沙箱零出站，装不下认知运行时：executor 必须 isolated。
			// executor 只驱动 Run，不向 Registry 提供 Capability。
			if component.Mode != ModeIsolated || len(component.Exports) > 0 {
				return ErrInvalidFormat
			}
		default:
			return ErrInvalidFormat
		}
		if !IsPackageEntrypoint(component.Entrypoint) ||
			component.Entrypoint == "manifest.json" || component.Entrypoint == "lock.json" {
			return ErrInvalidFormat
		}
		if (component.Mode == ModeHosted && component.Process != nil) ||
			(component.Mode == ModeIsolated && component.Process == nil) {
			return ErrInvalidFormat
		}
		if component.Mode != ModeHosted && len(component.HostFunctions) > 0 {
			return ErrInvalidFormat
		}
		if component.Process != nil {
			if err := ValidateProcessTemplate(*component.Process); err != nil {
				return ErrInvalidFormat
			}
		}
		for _, id := range append(append([]string(nil), component.Exports...), component.Imports...) {
			if !capability.IsStableID(id) {
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

// ValidateProcessTemplate 校验 isolated 组件的相对启动模板。
func ValidateProcessTemplate(template ProcessTemplate) error {
	if !IsPackagePath(template.Path) || template.Path == "." ||
		(template.WorkDir != "" && !IsPackagePath(template.WorkDir)) ||
		(template.Address != "" && !IsLocalRuntimeAddress(template.Address)) ||
		len(template.Args) > 128 {
		return ErrInvalidFormat
	}
	for _, argument := range template.Args {
		if len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return ErrInvalidFormat
		}
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
			providerIndex := indexByID[provider]
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
		lock.PackageVersion != manifest.Version || !capability.IsStableID(lock.PackageID) {
		return ErrInvalidFormat
	}
	if _, err := ParseVersion(lock.PackageVersion); err != nil {
		return ErrInvalidFormat
	}
	if !IsSHA256Hex(lock.ManifestSHA256) {
		return ErrInvalidFormat
	}
	if len(lock.Artifacts) != len(manifest.Components) {
		return ErrInvalidFormat
	}
	seen := make(map[string]struct{}, len(lock.Artifacts))
	for _, artifact := range lock.Artifacts {
		component, ok := FindComponent(manifest, artifact.ComponentID)
		if !ok {
			return ErrInvalidFormat
		}
		if _, duplicate := seen[artifact.ComponentID]; duplicate {
			return ErrInvalidFormat
		}
		seen[artifact.ComponentID] = struct{}{}
		if !IsSHA256Hex(artifact.SHA256) {
			return ErrInvalidFormat
		}
		if !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path {
			return ErrInvalidFormat
		}
		// 工件按 basename 平铺安装，lock 的根路径必须与清单声明的 entrypoint 一致：
		// 否则 lock 可以把摘要绑到包目录外任意一个绝对路径文件上。
		if filepath.Base(artifact.Path) != filepath.Base(component.Entrypoint) {
			return ErrInvalidFormat
		}
		switch component.Mode {
		case ModeHosted:
			if artifact.Process != nil {
				return ErrInvalidFormat
			}
		case ModeIsolated:
			if artifact.Process == nil ||
				!processPathBelongsToArtifact(artifact.Path, artifact.Process.Path) ||
				!pathWithin(filepath.Dir(artifact.Path), artifact.Process.WorkDir) {
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

// ValidateArchiveLock 校验发布 tarball 内的相对路径 lock。归档 lock 只绑定
// manifest 与发布工件摘要；安装器在目标目录内重新生成带绝对路径和进程规格的
// 安装 lock，不能把归档 lock 当作安装 lock 直接执行。
func ValidateArchiveLock(lock Lock, manifest Manifest) error {
	if lock.SchemaVersion != SchemaVersion || lock.PackageID != manifest.ID ||
		lock.PackageVersion != manifest.Version || !capability.IsStableID(lock.PackageID) ||
		!IsSHA256Hex(lock.ManifestSHA256) || len(lock.Artifacts) != len(manifest.Components) {
		return ErrInvalidFormat
	}
	seen := make(map[string]struct{}, len(lock.Artifacts))
	for _, artifact := range lock.Artifacts {
		component, ok := FindComponent(manifest, artifact.ComponentID)
		if !ok || !IsPackageEntrypoint(artifact.Path) || artifact.Path != component.Entrypoint ||
			artifact.Process != nil || !IsSHA256Hex(artifact.SHA256) {
			return ErrInvalidFormat
		}
		if _, duplicate := seen[artifact.ComponentID]; duplicate {
			return ErrInvalidFormat
		}
		seen[artifact.ComponentID] = struct{}{}
	}
	return nil
}

// processPathBelongsToArtifact 限制 isolated 进程可执行文件在组件工件内；
// 文件工件要求进程路径就是工件本身，目录工件允许其内部的解释器或启动器。
func processPathBelongsToArtifact(artifactPath, processPath string) bool {
	return pathWithin(artifactPath, processPath)
}

// pathWithin 判断 candidate 是否位于 root 内，包含 root 本身。
func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// IsPackageEntrypoint 校验包根目录下的扁平工件路径。
func IsPackageEntrypoint(value string) bool {
	return IsPackagePath(value) && value != "." && !strings.ContainsRune(value, '/')
}

// IsSHA256Hex 判断字符串是否为合法十六进制 SHA-256 摘要。只校验长度会让 64 个
// "g" 这类非十六进制垃圾通过 lock 校验，摘要比对之前就该拒掉。
func IsSHA256Hex(digest string) bool {
	raw, err := hex.DecodeString(digest)
	return err == nil && len(raw) == sha256.Size
}

// FindComponent 按稳定 ID 查找一个包组件。
func FindComponent(manifest Manifest, id string) (Component, bool) {
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
		if strings.HasPrefix(socketPath, "/") {
			return path.Clean(socketPath) == socketPath
		}
		return filepath.IsAbs(socketPath) && filepath.Clean(socketPath) == socketPath
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
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
