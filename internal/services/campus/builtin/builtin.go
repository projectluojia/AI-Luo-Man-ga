// Package builtin 承载 campus hosted 包的嵌入工件副本（测试装配用）。
// 生产工件由独立包仓库 ailuo-packages/campus-bus 经 ailuo pack [build]
// 交叉编译后经 ailuo install 安装；本 go:embed 副本仅供 campustest 测试
// 装配使用，与生产形态一致但不可自动同步——更新 guest 后需在此重新嵌入。
package builtin

import (
	_ "embed"
)

// CampusWASM 是 campus hosted 包的 wasm32-wasi 工件。
//
//go:embed campus.wasm
var CampusWASM []byte
