// Package projectcontract 定义 AI珞 项目依赖清单与解析锁的公共契约。
//
// 项目清单描述应用需要哪些包；项目锁记录解析后的完整包闭包及每个已安装
// 包的完整性摘要。包管理器负责解析和安装，Core 只读取并校验锁定结果。
package projectcontract

import (
	"errors"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// SchemaVersion 是项目 manifest 与 lock 共用的格式版本。
const SchemaVersion = "ailuo.project.v1"

// MaxLockBytes 限制项目锁文件大小，避免启动时读取不受限输入。
const MaxLockBytes = int64(64 << 10)

const (
	MaxDirectDependencies = 256
	MaxLockedPackages     = 256
)

var ErrInvalid = errors.New("invalid project package contract")

// Manifest 是项目级依赖清单，对应项目根目录的 ailuo.toml。
type Manifest struct {
	SchemaVersion string       `json:"schema_version"`
	ID            string       `json:"id"`
	Dependencies  []Dependency `json:"dependencies,omitempty"`
}

// Dependency 是项目或包的直接依赖：来源必须显式，避免解析器猜测注册表或
// 从当前安装集合静默补齐。
type Dependency struct {
	ID         string `json:"id"`
	Constraint string `json:"constraint"`
	Source     string `json:"source"`
}

// Lock 是项目解析锁，Packages 必须包含项目依赖的完整传递闭包。
type Lock struct {
	SchemaVersion         string          `json:"schema_version"`
	ProjectID             string          `json:"project_id"`
	ProjectManifestSHA256 string          `json:"project_manifest_sha256"`
	Packages              []LockedPackage `json:"packages"`
}

// LockedPackage 是项目锁中的一个精确包版本。ManifestSHA256 和 LockSHA256
// 分别绑定安装目录内的 manifest.json 与 lock.json，Core 启动时重新计算。
type LockedPackage struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Source         string `json:"source"`
	ManifestSHA256 string `json:"manifest_sha256"`
	LockSHA256     string `json:"lock_sha256"`
}

// ValidateManifest 校验项目依赖清单的结构与唯一性。
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion || !capability.IsStableID(manifest.ID) ||
		len(manifest.Dependencies) > MaxDirectDependencies {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(manifest.Dependencies))
	for _, dependency := range manifest.Dependencies {
		if err := validateDependency(dependency); err != nil {
			return err
		}
		if _, exists := seen[dependency.ID]; exists {
			return ErrInvalid
		}
		seen[dependency.ID] = struct{}{}
	}
	return nil
}

// ValidateLockShape 校验项目锁本身的结构。直接依赖是否全部出现、传递闭包
// 是否完整以及约束是否满足由解析器结合项目清单验证。
func ValidateLockShape(lock Lock) error {
	if lock.SchemaVersion != SchemaVersion || !capability.IsStableID(lock.ProjectID) ||
		len(lock.Packages) > MaxLockedPackages ||
		!packagecontract.IsSHA256Hex(lock.ProjectManifestSHA256) {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(lock.Packages))
	for _, locked := range lock.Packages {
		if !capability.IsStableID(locked.ID) || locked.ID == lock.ProjectID ||
			!validVersion(locked.Version) ||
			packagecontract.ValidateSource(locked.Source) != nil ||
			!packagecontract.IsSHA256Hex(locked.ManifestSHA256) ||
			!packagecontract.IsSHA256Hex(locked.LockSHA256) {
			return ErrInvalid
		}
		if _, exists := seen[locked.ID]; exists {
			return ErrInvalid
		}
		seen[locked.ID] = struct{}{}
	}
	return nil
}

// ValidateLock 校验项目锁的结构，并确保每个直接依赖都有锁定项。传递闭包
// 的完整性（没有额外不可达包、包依赖都能在锁中满足）由解析器验证。
func ValidateLock(lock Lock, manifest Manifest) error {
	if err := ValidateManifest(manifest); err != nil || lock.ProjectID != manifest.ID ||
		ValidateLockShape(lock) != nil {
		return ErrInvalid
	}
	lockedByID := make(map[string]LockedPackage, len(lock.Packages))
	for _, locked := range lock.Packages {
		lockedByID[locked.ID] = locked
	}
	for _, dependency := range manifest.Dependencies {
		locked, exists := lockedByID[dependency.ID]
		if !exists || locked.Source != dependency.Source {
			return ErrInvalid
		}
		constraint, err := packagecontract.ParseConstraint(dependency.Constraint)
		if err != nil {
			return ErrInvalid
		}
		version, err := packagecontract.ParseVersion(locked.Version)
		if err != nil || !constraint.Matches(version) {
			return ErrInvalid
		}
	}
	return nil
}

func validateDependency(dependency Dependency) error {
	if !capability.IsStableID(dependency.ID) {
		return ErrInvalid
	}
	if _, err := packagecontract.ParseConstraint(dependency.Constraint); err != nil {
		return ErrInvalid
	}
	if packagecontract.ValidateSource(dependency.Source) != nil {
		return ErrInvalid
	}
	return nil
}

func validVersion(value string) bool {
	_, err := packagecontract.ParseVersion(value)
	return err == nil
}
