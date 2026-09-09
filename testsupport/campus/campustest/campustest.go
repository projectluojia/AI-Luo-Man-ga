// Package campustest 提供校园 App hosted 装配的共享测试辅助。
//
// 装配机制委托共享的 hostedtest：不做 guest 源码复制，读仓库内真实
// packages/campus-bus 清单（ailuo.toml）与源码（src/、bus/、guestkit），
// packagefmt go-wasm 现场编译——清单与 guest 的测试即生产形态。校巴数据由
// 测试经 packstore.Store 播种（App 隔离由 Scope 强制），guest 经 ailuo.store
// 宿主函数读取，完整链路与生产安装包一致。
package campustest

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// RegisterHosted 以 hosted 包形态装配 campus-bus 包：真实清单 + 真实 guest
// 源码，ailuo.store 宿主函数绑定到 packstore 端口（App 隔离由 Scope 强制），
// 与生产安装包链路一致。
func RegisterHosted(t testing.TB, target *registry.Registry, store packstore.Store) {
	t.Helper()
	hostedtest.Register(t, target, store, hostedtest.Spec{
		Dir: "campus-bus", ComponentID: campus.BusComponentID, StorageNamespace: campus.StorageNamespace,
	})
}
