// Package sqlitetest 提供测试存储的关闭与清理辅助：modernc SQLite 在 Windows
// 上以 sqlite3_close_v2 语义延迟释放文件句柄，且新创建的数据库文件可能被外部
// 进程（如 Defender 实时扫描）短暂锁定，直接依赖 t.TempDir 清理会与这些时序
// 竞争导致 RemoveAll 失败（仓库 #7 的 closeStore 模式共享版）。
package sqlitetest

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// NewStore 创建测试存储：使用自管临时目录（不经 t.TempDir，避免框架清理对
// 外部锁竞争二次报错），并注册关闭与尽力而为的目录清理。
func NewStore(t *testing.T, name string) *sqlite.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "ailuo-sqlite-test-")
	if err != nil {
		t.Fatalf("create sqlite temp dir: %v", err)
	}
	store, err := sqlite.Open(filepath.Join(dir, name))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { CloseAndWait(t, store, dir) })
	return store
}

// CloseAndWait 关闭测试存储并等待文件句柄释放，随后删除临时目录：
// 反复 GC + 重试直到目录可删（上限 10 秒）。删除是卫生问题而非正确性问题：
// 新建数据库文件可能被外部进程（如 Defender 实时扫描）锁定，超出重试上限时
// 只记录日志、不判定测试失败——残留目录位于系统临时空间，无害。
func CloseAndWait(t *testing.T, store *sqlite.Store, dir string) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Errorf("close store: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("测试 SQLite 临时目录删除被外部锁延迟，已放弃本次清理",
				"dir", dir,
				"reason", err.Error(),
			)
			return
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
