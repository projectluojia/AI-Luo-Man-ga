//go:build wasip1

// classroom 是空闲教室 Provider 的 hosted 包 guest：6 个 Capability 的
// 薄装配层。业务逻辑在 classroom/（领域模型）与 app/（分发），ailuo.store
// 访问统一走 guestkit——本文件只装配 stdin/stdout 循环。
package main

import (
	"os"

	"github.com/projectluojia/classroom/app"
	"github.com/projectluojia/guestkit"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
