//go:build !windows

package sqlite

import (
	"os"
	"path/filepath"
)

// publishDatabaseFile 以硬链接发布数据库文件：先链接到目标再删除临时文件，
// 最后同步目录；Windows 使用 MOVEFILE_WRITE_THROUGH 路径（publish_file_windows.go）。
func publishDatabaseFile(temporary, destination string) error {
	if err := os.Link(temporary, destination); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}
