package loader

import (
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
)

// 包声明扩展：宿主函数依赖与持久化契约。两者都是"声明"，不是执行凭据：
// 宿主函数实现永远由宿主（内核 Go 代码）提供，包只声明需要哪些投影；
// storage 只是包声明的持久化命名空间契约，凭据与连接由宿主托管。

var (
	errInvalidHostedFunctionDecl = errors.New("invalid hosted function declaration")
	hostFunctionPattern          = id.StableLower
	storageNamespacePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[/._:-][a-z0-9]+)*$`)
)

// 敏感级别与保留策略的闭式取值。
const (
	SensitivityPublic  = "public"
	SensitivityPrivate = "private"
	RetentionPermanent = "permanent"
	RetentionTemporary = "temporary"
)

// HostedFunctionDecl 是包声明的宿主函数依赖：guest 只可调用清单声明且宿主
// 注册表提供的宿主函数（module.name）。WASI 模块由沙箱自动提供，不可声明。
type HostedFunctionDecl struct {
	Module  string `json:"module"`
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`
}

// InstalledStorage 是包声明的持久化契约：无状态包实例经该命名空间读写宿主
// 统一存储；schema 只做前向迁移（AGENTS.md 存储规则）。
type InstalledStorage struct {
	Namespace     string `json:"namespace"`
	SchemaVersion uint32 `json:"schema_version"`
	Sensitivity   string `json:"sensitivity"`
	Retention     string `json:"retention"`
}

// validateHostedFunctionDecls 校验声明集合：标识符闭式、去重、用途长度，
// 且不得声明 WASI 模块。
func validateHostedFunctionDecls(decls []HostedFunctionDecl) error {
	seen := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		if !hostFunctionPattern.MatchString(decl.Module) || !hostFunctionPattern.MatchString(decl.Name) ||
			decl.Module == wasiModuleName || len(decl.Purpose) > 256 || !utf8.ValidString(decl.Purpose) {
			return errInvalidHostedFunctionDecl
		}
		for _, character := range decl.Purpose {
			if character < 0x20 || character == 0x7f {
				return errInvalidHostedFunctionDecl
			}
		}
		key := hostedFunctionKey(decl.Module, decl.Name)
		if _, exists := seen[key]; exists {
			return errInvalidHostedFunctionDecl
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateInstalledStorage 校验持久化契约声明的闭式取值。
func validateInstalledStorage(storage InstalledStorage) error {
	if !storageNamespacePattern.MatchString(storage.Namespace) || len(storage.Namespace) > 128 ||
		storage.SchemaVersion == 0 ||
		(storage.Sensitivity != SensitivityPublic && storage.Sensitivity != SensitivityPrivate) ||
		(storage.Retention != RetentionPermanent && storage.Retention != RetentionTemporary) {
		return ErrInstallCatalogInvalid
	}
	return nil
}

// hostedFunctionKey 返回宿主函数声明/实现的唯一键。
func hostedFunctionKey(module, name string) string {
	return module + "." + name
}

// equalHostedFunctionDecls 比较声明集合（忽略用途文本：用途只作说明，不参与身份）。
func equalHostedFunctionDecls(left, right []HostedFunctionDecl) bool {
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

func cloneHostedFunctionDecls(decls []HostedFunctionDecl) []HostedFunctionDecl {
	return append([]HostedFunctionDecl(nil), decls...)
}
