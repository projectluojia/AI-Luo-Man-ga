// Package packagecontract 是 AI珞 包格式的版本化公共契约：semver、包清单、声明与
// 校验。本包不引用任何内核包，除 semver 约束求解（github.com/Masterminds/semver/v3）
// 外只依赖标准库，是可整体迁移到独立仓库的包管理器基底；宿主（AI珞 内核）在
// 装载时解释包清单中的宿主扩展段。
package packagecontract

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

// 包格式版本：清单与 lock 共用的 schema 版本。
const SchemaVersion = "ailuo.package.v3"

// Package 清单的集合上限：安装器、Catalog 和运行时注册共享这些边界，
// 防止一个合法 JSON 通过超大依赖图耗尽装载资源。
const (
	MaxComponents   = 64
	MaxCapabilities = 256
	MaxDependencies = 256
)

// 执行形态与托管策略的闭式取值。
const (
	ModeHosted   = "hosted"
	ModeIsolated = "isolated"
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

// Storage 是包声明的持久化契约：无状态包实例经该命名空间读写宿主统一存储。
type Storage struct {
	Namespace string `json:"namespace"`
}

// Dependency 是包声明的版本化依赖（包 ID + semver 约束 + 显式来源）。
type Dependency struct {
	ID         string `json:"id"`
	Constraint string `json:"constraint"`
	Source     string `json:"source"`
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
	if !storageNamespacePattern.MatchString(storage.Namespace) || len(storage.Namespace) > 128 {
		return ErrInvalidFormat
	}
	return nil
}

// EqualStorage 比较持久化契约声明（nil 与缺失等价）。
func EqualStorage(left, right *Storage) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// ValidateDependency 校验依赖声明：标识符、来源闭式且 semver 约束可解析。
func ValidateDependency(dep Dependency) error {
	if !capability.IsStableID(dep.ID) || !validSource(dep.Source) {
		return ErrInvalidFormat
	}
	if _, err := ParseConstraint(dep.Constraint); err != nil {
		return ErrInvalidFormat
	}
	return nil
}

// ValidateSource 校验依赖来源的公共表示。path: 表示包管理器工作区内的相对
// 路径，github: 表示 GitHub owner/repo；解析和下载由包管理器负责，Core 只
// 消费已经解析并锁定的结果。
func ValidateSource(source string) error {
	if !validSource(source) {
		return ErrInvalidFormat
	}
	return nil
}

func validSource(source string) bool {
	if source == "" || len(source) > 512 || strings.TrimSpace(source) != source ||
		!utf8.ValidString(source) || strings.ContainsRune(source, '\x00') {
		return false
	}
	switch {
	case strings.HasPrefix(source, "path:"):
		value := strings.TrimPrefix(source, "path:")
		return value != "." && IsPackagePath(value)
	case strings.HasPrefix(source, "github:"):
		parts := strings.Split(strings.TrimPrefix(source, "github:"), "/")
		return len(parts) == 2 && validSourceSegment(parts[0]) && validSourceSegment(parts[1])
	default:
		return false
	}
}

func validSourceSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			character != '.' && character != '-' && character != '_' {
			return false
		}
	}
	return true
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
