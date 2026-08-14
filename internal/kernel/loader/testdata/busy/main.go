// busy 是 hosted 执行时间预算的测试工件：进入死循环永不返回。
// 用于验证 WasmHost 在预算耗尽时强制终止 guest（WithCloseOnContextDone）。
// 只用于 internal/kernel/loader 测试，非产品代码。
package main

func main() {
	for {
	}
}
