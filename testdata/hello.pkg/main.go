//go:build wasip1

// hello 说你好。
package main

// hello 是 hello.pkg 的唯一 capability。
func hello(args HelloArgs) {}

// HelloArgs 是 hello 的输入。
type HelloArgs struct {
	Name string `json:"name"`
}

func main() {}
