package loader_test

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// Loader hosted 测试契约：宿主侧集中持有的稳定测试标识。
// 测试固件（testdata/{success,hostfn,busy}）不知道包名与能力名；如果测试需要
// 新增标识，在此集中声明，避免字面量在测试间漂移。
const (
	// testPackageID 是 hosted 测试包的 Package/Runtime 标识。
	testPackageID = "runtime.test"
	// testInvokeCapabilityID 是 hosted 测试包直接调用的能力标识。
	testInvokeCapabilityID = "runtime.test.echo"
	// testCapabilityID 是测试包对外暴露的能力标识。
	testCapabilityID = "runtime.test.echo.cap"
	// testInvokeScope 是 hosted 测试包调用的权限范围。
	testInvokeScope = "runtime.test.invoke"
)

// staticHostFunctions 返回对所有清单提供同一组宿主函数的配置助手。
func staticHostFunctions(functions ...loader.HostedFunction) func(loader.Manifest) ([]loader.HostedFunction, error) {
	return func(loader.Manifest) ([]loader.HostedFunction, error) {
		return functions, nil
	}
}
