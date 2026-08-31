package qq

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// newQQTestStore 打开内存态测试存储并注册清理。不用临时目录文件：modernc
// SQLite 在 Windows 上对回显链路产生的句柄存在延迟释放，文件删除会与
// finalizer 竞争导致 TempDir 清理偶发失败（CI flaky）；内存库语义相同且无
// 文件句柄竞争。每个 :memory: 打开都是独立数据库，测试间隔离不变。
func newQQTestStore(t *testing.T, _ string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}
