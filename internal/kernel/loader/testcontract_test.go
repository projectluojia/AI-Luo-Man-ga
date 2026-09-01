package loader_test

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// Loader hosted 测试契约：宿主侧集中持有的稳定测试标识。
// 测试固件（testdata/{success,hostfn,busy}）不知道包名与工具名；如果测试需要
// 新增标识，在此集中声明，避免字面量在测试间漂移。
const (
	// testPackageID 是 hosted 测试包的 Package/Runtime 标识。
	testPackageID = "runtime.test"
	// testToolID 是 hosted 测试包声明的原子工具标识。
	testToolID = "runtime.test.echo"
	// testInvokeScope 是 hosted 测试包调用的权限范围。
	testInvokeScope = "runtime.test.invoke"
)

// staticHostFunctions 返回对所有清单提供同一组宿主函数的配置助手。
func staticHostFunctions(functions ...loader.HostedFunction) func(loader.Manifest) ([]loader.HostedFunction, error) {
	return func(loader.Manifest) ([]loader.HostedFunction, error) {
		return functions, nil
	}
}
