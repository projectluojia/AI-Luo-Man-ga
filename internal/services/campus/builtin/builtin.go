// Package builtin 承载 campus hosted 包的嵌入工件副本（测试装配用）：
// 生产工件由 extensions/campus.bus 经 ailuo pack [build] 交叉编译输出，
// 安装目录装载；本 go:embed 副本仅供 campustest 测试装配使用。
package builtin

import (
	_ "embed"
)

// CampusWASM 是 campus hosted 包的 wasm32-wasi 工件。
//
//go:embed campus.wasm
var CampusWASM []byte
