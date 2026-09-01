package sdkgen

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/iancoleman/strcase"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

// reservedKeywords 是 Python 与 TypeScript 保留字的并集：字段名直接作为标识符
// （Python dataclass 属性 / TS interface 属性），撞保留字会生成非法代码 → 显式
// 拒绝（fail-closed），不自动改名（改名会破坏 JSON key 与契约的一致性）。
// 取并集而非按目标语言分辨：契约字段名对三语言同时可用才算合法。
var reservedKeywords = stringSet(
	"False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield " +
		"case catch const debugger default delete do enum export extends false function instanceof new null super switch this throw true typeof var void implements interface let package private protected public static")

// validateCapabilityID 校验 capability ID 符合公共契约格式。
func validateCapabilityID(id string) error {
	if !capability.IsStableID(id) {
		return fmt.Errorf("sdkgen: capability id %q 不合法（需小写字母/数字，点/下划线/连字符分段）", id)
	}
	return nil
}

func validateDerivedNames(capabilities []capability.CapabilitySpec, packageID string) error {
	seenTypes := make(map[string]string, len(capabilities))
	seenGoMethods := make(map[string]string, len(capabilities))
	seenPythonMethods := make(map[string]string, len(capabilities))
	seenTSMethods := make(map[string]string, len(capabilities))
	for _, capability := range capabilities {
		for _, names := range []struct {
			kind string
			name string
			seen map[string]string
		}{
			{kind: "type", name: typeName(capability.ID, packageID), seen: seenTypes},
			{kind: "Go method", name: goMethodName(capability.ID, packageID), seen: seenGoMethods},
			{kind: "Python method", name: pythonMethodName(capability.ID, packageID), seen: seenPythonMethods},
			{kind: "TypeScript method", name: tsMethodName(capability.ID, packageID), seen: seenTSMethods},
		} {
			if previous, exists := names.seen[names.name]; exists {
				return fmt.Errorf("sdkgen: %s 名称 %q 在 capability %q 与 %q 间冲突",
					names.kind, names.name, previous, capability.ID)
			}
			names.seen[names.name] = capability.ID
		}
	}
	return nil
}

// identifierPattern 是 Python/TS 可直接作标识符的字段名形状。JSON key 允许
// "user-name"、"2fa"、"a b"，但它们作 dataclass 属性/interface 属性都是语法
// 错误，生成阶段就必须拒绝。
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateFieldNames 校验 object 字段名可直接作 Python/TS 标识符，且不是保留字。
// Go 字段名经 goFieldName 转导出名（首字母大写、按分隔符分段），不受影响。
func validateFieldNames(model *TypeModel) error {
	for _, namedModel := range collectNamed(model) {
		if namedModel.Kind != KindObject {
			continue
		}
		for _, field := range namedModel.Fields {
			if !identifierPattern.MatchString(field.Name) {
				return fmt.Errorf("sdkgen: 字段名 %q 不是合法标识符（需字母/下划线开头，仅含字母数字下划线）", field.Name)
			}
			if _, reserved := reservedKeywords[field.Name]; reserved {
				return fmt.Errorf("sdkgen: 字段名 %q 是 Python/TypeScript 保留字，请调整契约字段名", field.Name)
			}
		}
	}
	return nil
}

// stringSet 构造 O(1) 查找的字符串集合。
func stringSet(values string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, value := range strings.Fields(values) {
		set[value] = struct{}{}
	}
	return set
}

// 命名层：所有跨语言命名统一经 strcase（成熟命名库）转换，消除各 emitter
// 手写重复。唯一例外是 Go 字段名（goFieldName）：strcase 会把 "id" 段转成
// "Id" 而非 Go 惯例的 "ID"，故字段名保留逐段特判（json tag 才是契约，字段
// 名是内部标识）。

// typeName 派生 capability 输入类型名（campus.bus.stops.search → BusStopsSearchInput）。
func typeName(capabilityID, packageID string) string {
	return strcase.ToCamel(stripPackagePrefix(capabilityID, packageID)) + "Input"
}

// goMethodName 派生 Go 调用方法名（bus.stops.search → BusStopsSearch）。
func goMethodName(capabilityID, packageID string) string {
	return strcase.ToCamel(stripPackagePrefix(capabilityID, packageID))
}

// pythonMethodName 派生 Python 调用函数名（bus.stops.search → bus_stops_search）。
func pythonMethodName(capabilityID, packageID string) string {
	return strcase.ToSnake(stripPackagePrefix(capabilityID, packageID))
}

// tsMethodName 派生 TypeScript 调用方法名（bus.stops.search → busStopsSearch）。
func tsMethodName(capabilityID, packageID string) string {
	return strcase.ToLowerCamel(stripPackagePrefix(capabilityID, packageID))
}

// stripPackagePrefix 去掉 capability ID 的包前缀（"campus."），无前缀时原样保留。
func stripPackagePrefix(capabilityID, packageID string) string {
	prefix := packageID + "."
	if strings.HasPrefix(capabilityID, prefix) {
		return strings.TrimPrefix(capabilityID, prefix)
	}
	return capabilityID
}

// goFieldName 将 snake_case JSON 键转为 Go 导出字段名（depart_after → DepartAfter，
// 末尾 id 段 → ID）。strcase 会把 id 段转成 Id，故逐段特判。
func goFieldName(name string) string {
	parts := strings.Split(name, "_")
	var builder strings.Builder
	for _, part := range parts {
		if part == "id" {
			builder.WriteString("ID")
		} else {
			builder.WriteString(strcase.ToCamel(part))
		}
	}
	return builder.String()
}
