//go:build wasip1

// timetable 是课表 Provider 的 hosted 包 guest：12 个 Capability 的薄分发层。
// 业务约束在 packages/timetable/app，领域解析在 packages/timetable/tt，
// ailuo.store 访问统一走 guestkit——本文件只装配 stdin/stdout 循环。
package main

import (
	"os"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/timetable/app"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
