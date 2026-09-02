package packageio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// 文件完整性原语只负责格式与内容；安装目录的安全策略由
// ValidateSecurePath/ValidateSecureTree 统一提供。

// ReadFileLimited 受限读取单文件：常规文件、非符号链接、大小受限，并在读取
// 后复核同一文件（防 TOCTOU 替换）。
func ReadFileLimited(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maximum {
		return nil, packagecontract.ErrInvalidFormat
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, packagecontract.ErrInvalidFormat
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) > maximum {
		return nil, packagecontract.ErrInvalidFormat
	}
	return payload, nil
}

// HashFile 计算文件 SHA-256：常规文件、大小受限，读取期间复核同一文件。
func HashFile(ctx context.Context, path string, maximum int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return "", packagecontract.ErrInvalidFormat
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", packagecontract.ErrInvalidFormat
	}
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > maximum {
				return "", packagecontract.ErrInvalidFormat
			}
			if _, err := digest.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
