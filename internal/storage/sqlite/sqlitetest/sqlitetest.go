// Package sqlitetest 提供测试存储的关闭与清理辅助：modernc SQLite 在 Windows
// 上以 sqlite3_close_v2 语义延迟释放文件句柄，直接依赖 t.TempDir 清理会与
// 最终化时序竞争导致 RemoveAll 失败（仓库 #7 的 closeStore 模式共享版）。
package sqlitetest

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// CloseAndWait 关闭测试存储并等待文件句柄释放，随后删除临时目录：
// 反复 GC + 重试直到目录可删（上限 10 秒），避免 Windows 上 TempDir 清理失败。
// 上限取 10 秒：负载高的 CI 机器上 modernc 句柄延迟释放可能明显超过 1 秒
// （QQ 接入测试在 Windows runner 上曾连续超限），重试上限不足会误报失败。
func CloseAndWait(t *testing.T, store *sqlite.Store, dir string) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Errorf("close store: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := os.RemoveAll(dir); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Errorf("remove sqlite temp dir: %v", err)
			return
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
