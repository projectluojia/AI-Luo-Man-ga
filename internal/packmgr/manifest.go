package packmgr

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
)

// Manifest 是中立的包清单——语言与宿主无关的核心字段，宿主扩展段以原始 JSON
// 保留（如 AI珞 内核的 tools/service/capabilities 由内核解释）。
type Manifest struct {
	SchemaVersion string               `json:"schema_version"`
	ID            string               `json:"id"`
	Version       string               `json:"version"`
	Mode          string               `json:"mode"`
	Pin           bool                 `json:"pin,omitempty"`
	IdleTTLMS     uint64               `json:"idle_ttl_ms,omitempty"`
	Entrypoint    string               `json:"entrypoint,omitempty"`
	HostFunctions []HostedFunctionDecl `json:"host_functions,omitempty"`
	Storage       *Storage             `json:"storage,omitempty"`
	Dependencies  []Dependency         `json:"dependencies,omitempty"`
	// Extensions 是宿主扩展段（json.RawMessage），原样保留给宿主解释：
	// AI珞 内核期望 "{"tools":[...],"service":{...},"capabilities":[...]}"。
	Extensions json.RawMessage `json:"extensions,omitempty"`
}

// Lock 是安装目录的锁定记录：固定解析版本与工件完整性，不可变（npm 式）。
type Lock struct {
	SchemaVersion  string       `json:"schema_version"`
	PackageID      string       `json:"package_id"`
	PackageVersion string       `json:"package_version"`
	Mode           string       `json:"mode"`
	ManifestSHA256 string       `json:"manifest_sha256"`
	ArtifactSHA256 string       `json:"artifact_sha256"`
	ArtifactPath   string       `json:"artifact_path"`
	Process        *ProcessSpec `json:"process,omitempty"`
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

// InstalledRecord 是中立的安装目录记录，供宿主决定如何注册和装载。
type InstalledRecord struct {
	Directory    string
	ArtifactPath string
	Manifest     Manifest
	Process      *ProcessSpec
}

// ValidateManifest 校验清单的中性核心字段。宿主扩展段未在此层校验
// （宿主在解析 Extensions 时自行严格解码）。
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || !stableLowerPattern.MatchString(manifest.ID) ||
		len(manifest.ID) > 128 {
		return ErrInvalidFormat
	}
	if _, err := ParseVersion(manifest.Version); err != nil {
		return ErrInvalidFormat
	}
	if manifest.Mode != ModeHosted && manifest.Mode != ModeIsolated {
		return ErrInvalidFormat
	}
	if manifest.IdleTTLMS > 2592000000 { // 30 天
		return ErrInvalidFormat
	}
	if err := ValidateHostedFunctions(manifest.HostFunctions); err != nil {
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
	// 含有 Extensions 时至少为合法 JSON（宿主再严格解码语义）。
	if len(manifest.Extensions) > 0 && !json.Valid(manifest.Extensions) {
		return ErrInvalidFormat
	}
	return nil
}

// ValidateLock 校验锁定记录的内部一致性。
func ValidateLock(lock Lock) error {
	if lock.SchemaVersion != SchemaVersion || !stableLowerPattern.MatchString(lock.PackageID) ||
		len(lock.PackageID) > 128 {
		return ErrInvalidFormat
	}
	if _, err := ParseVersion(lock.PackageVersion); err != nil {
		return ErrInvalidFormat
	}
	if lock.Mode != ModeHosted && lock.Mode != ModeIsolated {
		return ErrInvalidFormat
	}
	if len(lock.ManifestSHA256) != 64 || len(lock.ArtifactSHA256) != 64 {
		return ErrInvalidFormat
	}
	if !filepath.IsAbs(lock.ArtifactPath) || filepath.Clean(lock.ArtifactPath) != lock.ArtifactPath {
		return ErrInvalidFormat
	}
	return nil
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
