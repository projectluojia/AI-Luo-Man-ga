package wasmhost_test

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader/wasmhost"
)

// hosted 测试契约：稳定测试标识（与 runtimehost/testcontract_test.go 同源，
// 两个测试包各自持有，避免跨包导出测试符号）。
const (
	// testPackageID 是 hosted 测试包的 Package/Runtime 标识。
	testPackageID = "runtime.test"
	// testInvokeCapabilityID 是 hosted 测试包直接调用的能力标识。
	testInvokeCapabilityID = "runtime.test.echo"
)

// digest 是测试清单锁定的占位工件摘要（WasmHost 校验走 ReadArtifact 侧）。
const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// staticHostFunctions 返回对所有清单提供同一组宿主函数的配置助手。
func staticHostFunctions(functions ...wasmhost.HostedFunction) func(loader.Manifest) ([]wasmhost.HostedFunction, error) {
	return func(loader.Manifest) ([]wasmhost.HostedFunction, error) {
		return functions, nil
	}
}
