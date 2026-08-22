package packmgr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Pack 把源包目录打成可分发 tarball（npm pack 等价物）：校验清单与工件后
// 生成 `<id>-<version>.tgz`，条目为扁平布局（manifest.json + 工件），供
// 注册表发布与 `ailuo install <pkg>.tgz` 安装。
func Pack(ctx context.Context, sourceDir, outputDir string) (string, error) {
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return "", err
	}
	tarballPath := filepath.Join(outputDir, source.Manifest.ID+"-"+source.Manifest.Version+".tgz")
	file, err := os.OpenFile(tarballPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	writeEntry := func(name string, size int64, body io.Reader) error {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(tarWriter, body)
		return err
	}
	if err := writeEntry("manifest.json", int64(len(source.manifestBytes)), bytes.NewReader(source.manifestBytes)); err != nil {
		return "", err
	}
	artifact, err := os.Open(source.artifactPath)
	if err != nil {
		return "", err
	}
	defer artifact.Close()
	info, err := artifact.Stat()
	if err != nil {
		return "", err
	}
	if err := writeEntry(filepath.Base(source.Manifest.Entrypoint), info.Size(), artifact); err != nil {
		return "", err
	}
	// tar 尾 + gzip 尾按序关闭；defer 的 file.Close 只负责文件描述符。
	if err := tarWriter.Close(); err != nil {
		return "", err
	}
	if err := gzipWriter.Close(); err != nil {
		return "", err
	}
	return tarballPath, nil
}

// unpackTarball 严格解压 tarball 到目标目录：拒绝绝对路径、路径穿越、
// 非普通文件/目录条目与超限大小（tarball 是外部输入，属于信任边界）。
func unpackTarball(tarball, dest string) error {
	file, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return ErrInvalidFormat
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ErrInvalidFormat
		}
		if !filepath.IsLocal(header.Name) {
			return fmt.Errorf("%w: tarball 条目路径非法 %q", ErrInvalidFormat, header.Name)
		}
		if header.Size < 0 || header.Size > MaxArtifactBytes {
			return ErrInvalidFormat
		}
		total += header.Size
		if total > MaxArtifactBytes {
			return ErrInvalidFormat // 解压总量上限，防解压炸弹
		}
		target := filepath.Join(dest, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				return err
			}
			// tar.Reader 按条目边界供给，超量读不到；不足声明大小（截断）报错。
			if _, err := io.CopyN(file, tarReader, header.Size); err != nil {
				file.Close()
				return ErrInvalidFormat
			}
			if err := file.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tarball 包含不支持条目 %q", ErrInvalidFormat, header.Name)
		}
	}
}
