//go:build wasip1

// campus 是校园能力 Provider 的 hosted 包 guest：4 个校巴 Capability 的
// 薄装配层。业务逻辑在 packages/campus-bus/bus，ailuo.store 访问统一走
// guestkit——本文件只装配 stdin/stdout 循环。
package main

import (
	"os"

	"github.com/projectluojia/campus-bus/bus"
	"github.com/projectluojia/guestkit"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, bus.Handlers(guestkit.NewStoreClient()))
}
