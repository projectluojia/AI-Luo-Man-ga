package sdkgen

import (
	"strings"

	"github.com/iancoleman/strcase"
)

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
