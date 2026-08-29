package qq

import (
	"context"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
)

// newQQTestStore 打开测试存储并注册清理：关闭后等待 Windows 文件句柄释放
// 再删目录（modernc SQLite 延迟释放，避免 TempDir 清理竞争）。
// runQQAdapterForTest 启动 adapter goroutine 并注册退出等待：清理时先取消
// 并等 Run 返回，再按 LIFO 执行后续的 store 关闭清理，消除 worker 仍在使用
// 连接时关闭存储的竞争（Windows 上表现为临时目录长时间无法删除）。
func runQQAdapterForTest(t *testing.T, run func(context.Context) error, ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("QQ adapter goroutine did not exit before store cleanup")
		}
	})
}

func newQQTestStore(t *testing.T, name string) *sqlite.Store {
	t.Helper()
	return sqlitetest.NewStore(t, name)
}
