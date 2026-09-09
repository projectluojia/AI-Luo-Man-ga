//go:build wasip1

// calendar 是学业校历 Provider 的 hosted 包 guest：单一 Capability 的薄装配
// 层。业务逻辑在 calendar/（领域模型）与 app/（分发），ailuo.store 访问统一
// 走 guestkit——本文件只装配 stdin/stdout 循环。
package main

import (
	"os"

	"github.com/projectluojia/calendar/app"
	"github.com/projectluojia/guestkit"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
