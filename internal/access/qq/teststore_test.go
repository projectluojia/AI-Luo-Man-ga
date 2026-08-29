package qq

import (
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
)

// newQQTestStore 打开测试存储并注册清理：关闭后等待 Windows 文件句柄释放
// 再删目录（modernc SQLite 延迟释放，避免 TempDir 清理竞争）。
func newQQTestStore(t *testing.T, name string) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, dir) })
	return store
}
