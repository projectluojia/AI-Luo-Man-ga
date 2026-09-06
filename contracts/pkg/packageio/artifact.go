package packageio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// HashArtifact 计算文件或目录工件的确定性 SHA-256。目录摘要包含相对路径、
// 文件类型和所有文件内容；符号链接、特殊文件和超限工件一律拒绝。
func HashArtifact(ctx context.Context, path string, maximum int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
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
