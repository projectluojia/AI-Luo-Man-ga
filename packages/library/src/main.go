//go:build wasip1

package main

import (
	"os"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/library/app"
)

func main() {
	guestkit.Run(os.Stdin, os.Stdout, app.Handlers(guestkit.NewStoreClient()))
}
