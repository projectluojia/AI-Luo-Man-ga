// Package builtin 承载 campus hosted 包的嵌入工件。
// 工件由 extensions/campus/build.ps1（或 build.sh）交叉编译后复制到此目录，
// 随内核二进制发布，保证三平台（含 Windows）均可装载。
package builtin

import (
	_ "embed"
)

// CampusWASM 是 campus hosted 包的 wasm32-wasi 工件。
//
//go:embed campus.wasm
var CampusWASM []byte
