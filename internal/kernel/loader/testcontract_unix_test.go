//go:build unix

package loader_test

const (
	// testRuntimeID 是 Unix Runtime Host 集成测试包内运行组件的稳定标识。
	testRuntimeID = testPackageID + ".runtime"
	// testCapabilityID 是 Unix Runtime Host 集成测试包对外暴露的能力标识。
	testCapabilityID = "runtime.test.echo.cap"
)
