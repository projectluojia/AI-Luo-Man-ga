package loader_test

// Loader hosted 测试契约：宿主侧集中持有的稳定测试标识。
// 测试固件（testdata/{success,hostfn,busy}）不知道包名与工具名；如果测试需要
// 新增标识，在此集中声明，避免字面量在测试间漂移。
const (
	// testPackageID 是 hosted 测试包的 Package/Runtime 标识。
	testPackageID = "runtime.test"
	// testToolID 是 hosted 测试包声明的原子工具标识。
	testToolID = "runtime.test.echo"
	// testCapabilityID 是 hosted 测试包导出的 Capability 标识。
	testCapabilityID = "runtime.test.echo.cap"
	// testInvokeScope 是 hosted 测试包调用的权限范围。
	testInvokeScope = "runtime.test.invoke"
	// testCoreRuntimeID 是安装目录发现产生的 Runtime 标识（<package>.<component>）。
	testCoreRuntimeID = testPackageID + ".core"
)
