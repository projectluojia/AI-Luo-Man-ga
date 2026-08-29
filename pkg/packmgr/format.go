// Package packmgr 是 AI珞 包管理器的中立包格式层：semver、包清单、声明与
// 校验。本包不引用任何内核包，除 semver 约束求解（github.com/Masterminds/semver/v3）
// 外只依赖标准库，是可整体迁移到独立仓库的包管理器基底；宿主（AI珞 内核）在
// 装载时解释包清单中的宿主扩展段。
package packmgr

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

// 包格式版本：清单与 lock 共用的 schema 版本。
const SchemaVersion = "ailuo.package.v2"

// 执行形态与托管策略的闭式取值。
const (
	ModeHosted   = "hosted"
	ModeIsolated = "isolated"

	SensitivityPublic  = "public"
	SensitivityPrivate = "private"
	RetentionPermanent = "permanent"
	RetentionTemporary = "temporary"
)

var (
	// ErrInvalidFormat 表示包格式（清单/lock/声明）非法。
	ErrInvalidFormat = errors.New("invalid package format")
)

var (
	storageNamespacePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[/._:-][a-z0-9]+)*$`)
)

// HostedFunctionDecl 是包声明的宿主函数依赖：guest 只可调用清单声明且宿主
// 提供的宿主函数（module.name）。WASI 模块由沙箱自动提供，不可声明。
type HostedFunctionDecl struct {
	Module  string `json:"module"`
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`
}

// Storage 是包声明的持久化契约：无状态包实例经该命名空间读写宿主统一存储；
// schema 只做前向迁移（AGENTS.md 存储规则）。
type Storage struct {
	Namespace     string `json:"namespace"`
	SchemaVersion uint32 `json:"schema_version"`
	Sensitivity   string `json:"sensitivity"`
	Retention     string `json:"retention"`
}

// Dependency 是包声明的版本化依赖（tool_id + semver 约束）。
type Dependency struct {
	ID         string `json:"id"`
	Constraint string `json:"constraint"`
}

// ValidateHostedFunctions 校验声明集合：标识符闭式、去重、用途长度，且不得
// 声明 WASI 模块。
func ValidateHostedFunctions(decls []HostedFunctionDecl) error {
	seen := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		if !capability.IsStableID(decl.Module) || !capability.IsStableID(decl.Name) ||
			decl.Module == "wasi_snapshot_preview1" || len(decl.Purpose) > 256 || !utf8.ValidString(decl.Purpose) {
			return ErrInvalidFormat
		}
		for _, character := range decl.Purpose {
			if character < 0x20 || character == 0x7f {
				return ErrInvalidFormat
			}
		}
		key := HostedFunctionKey(decl.Module, decl.Name)
		if _, exists := seen[key]; exists {
			return ErrInvalidFormat
		}
		seen[key] = struct{}{}
	}
	return nil
}

// ValidateStorage 校验持久化契约声明的闭式取值。
func ValidateStorage(storage Storage) error {
	if !storageNamespacePattern.MatchString(storage.Namespace) || len(storage.Namespace) > 128 ||
		storage.SchemaVersion == 0 ||
		(storage.Sensitivity != SensitivityPublic && storage.Sensitivity != SensitivityPrivate) ||
		(storage.Retention != RetentionPermanent && storage.Retention != RetentionTemporary) {
		return ErrInvalidFormat
	}
	return nil
}

// ValidateDependency 校验依赖声明：标识符闭式 + semver 约束可解析。
func ValidateDependency(dep Dependency) error {
	if !capability.IsStableID(dep.ID) {
		return ErrInvalidFormat
	}
	if _, err := ParseConstraint(dep.Constraint); err != nil {
		return ErrInvalidFormat
	}
	return nil
}

// HostedFunctionKey 返回宿主函数声明/实现的唯一键。分隔符用 NUL：module 与
// name 都允许含 `.`，用 `.` 拼接会让 {"a.b","c"} 与 {"a","b.c"} 撞成同一个键。
// 该键只作进程内查找，不序列化、不出现在任何契约里。
func HostedFunctionKey(module, name string) string {
	return module + "\x00" + name
}

// IsPackagePath 校验包内相对路径，使用与宿主平台无关的正斜杠语法。
// 点路径允许表示包根目录；其他路径不得含空段、.、..、反斜杠或卷标。
func IsPackagePath(value string) bool {
	if value == "." {
		return true
	}
	if value == "" || strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// EqualHostedFunctions 比较声明集合（忽略用途文本：用途只作说明，不参与身份）。
func EqualHostedFunctions(left, right []HostedFunctionDecl) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Module != right[index].Module || left[index].Name != right[index].Name {
			return false
		}
	}
	return true
}
