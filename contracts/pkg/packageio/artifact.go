package packageio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// HashArtifact 计算文件或目录工件的确定性 SHA-256。目录摘要包含相对路径、
// 文件类型和所有文件内容；符号链接、特殊文件和超限工件一律拒绝。
func HashArtifact(ctx context.Context, path string, maximum int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return "", packagecontract.ErrInvalidFormat
	}
	if IsIgnoredArtifactPath(filepath.Base(path)) {
		return "", packagecontract.ErrInvalidFormat
	}
	if info.Mode().IsRegular() {
		return HashFile(ctx, path, maximum)
	}
	if !info.IsDir() {
		return "", packagecontract.ErrInvalidFormat
	}

	digest := sha256.New()
	if _, err := digest.Write([]byte("directory\x00")); err != nil {
		return "", err
	}
	var total int64
	hasFile := false
	err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return packagecontract.ErrInvalidFormat
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			return packagecontract.ErrInvalidFormat
		}
		relative = filepath.ToSlash(relative)
		if IsIgnoredArtifactPath(relative) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			_, _ = digest.Write([]byte("dir\x00" + relative + "\x00"))
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil || !fileInfo.Mode().IsRegular() {
			return packagecontract.ErrInvalidFormat
		}
		hasFile = true
		total += fileInfo.Size()
		if total > maximum {
			return packagecontract.ErrInvalidFormat
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(fileInfo, opened) {
			_ = file.Close()
			return packagecontract.ErrInvalidFormat
		}
		_, _ = digest.Write([]byte("file\x00" + relative + "\x00"))
		_, copyErr := io.Copy(digest, io.LimitReader(file, maximum+1))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if total > maximum {
			return packagecontract.ErrInvalidFormat
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !hasFile {
		return "", packagecontract.ErrInvalidFormat
	}
	latest, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, latest) {
		return "", packagecontract.ErrInvalidFormat
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// IsIgnoredArtifactPath 判断目录工件中的开发缓存路径。它们不属于运行时
// 交付物，统一从哈希和归档中排除，避免本地工具缓存改变发布摘要。
func IsIgnoredArtifactPath(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch part {
		case ".git", ".hg", ".svn", "__pycache__", ".mypy_cache", ".pytest_cache", ".ruff_cache":
			return true
		}
	}
	return strings.HasSuffix(relative, ".pyc") || strings.HasSuffix(relative, ".pyo") || filepath.Base(relative) == ".DS_Store"
}
