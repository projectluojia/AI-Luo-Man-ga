//go:build wasip1

// 珞珈E卡双入口包 guest 入口：stdin 读调用信封，stdout 写结果信封。
package main

import (
	"os"

	"github.com/projectluojia/ecard/app"
	"github.com/projectluojia/guestkit"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
