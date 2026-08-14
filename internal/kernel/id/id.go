// Package id 提供全仓库统一的稳定标识正则族。
// 各层不得自行复制标识正则，避免语义漂移；所有闭式标识取值必须来自本包。
package id

import "regexp"

var (
	// StableMixed 大小写混合稳定标识（≤128 字符）：Run/确认/任务/会话等内部标识。
	StableMixed = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	// StableMixedUncapped 大小写混合稳定标识（不限长）：幂等键/Blob/校巴数据标识。
	StableMixedUncapped = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	// AppID 小写 App 标识（≤128 字符）。
	AppID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	// StableLower 小写稳定标识：Service/Tool/Capability/渠道/平台等业务标识。
	StableLower = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	// Permission 权限标识：小写 + 点/下划线/冒号/连字符。
	Permission = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._:-][a-z0-9]+)*$`)
)
