//go:build wasip1

// 运动场馆预约包 guest 入口：stdin 读调用信封，stdout 写结果信封。
package main

import (
	"os"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/sports/app"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
