package packmgr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Pack 把源包目录打成可分发 tarball（npm pack 等价物）：校验清单与工件后
// 生成 `<id>-<version>.tgz`，条目为扁平布局（manifest.json + lock.json + 工件），
// 供注册表发布与 `ailuo install <pkg>.tgz` 安装。
func Pack(ctx context.Context, sourceDir, outputDir string) (string, error) {
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return "", err
	}
	return PackFromSource(ctx, sourceDir, outputDir, source.Manifest, source.manifestBytes)
}

// PackFromSource 用调用方提供的清单（如从 ailuo.toml 源清单解析）打包：
// 校验清单与源目录工件，生成 tarball 并附带按工件 SHA-256 锁定的 lock.json。
// 与 Pack 的唯一区别是清单来源：本函数不读取 manifest.json，供作者侧源清单
// （packagefmt）与旧 manifest.json 路径共用同一打包实现。
func PackFromSource(ctx context.Context, sourceDir, outputDir string, manifest Manifest, manifestBytes []byte) (string, error) {
	if err := ValidateManifest(manifest); err != nil {
		return "", err
	}
	artifacts, err := readSourceArtifacts(sourceDir, manifest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return "", err
	}
	tarballPath := filepath.Join(outputDir, manifest.ID+"-"+manifest.Version+".tgz")
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
	if err := writeEntry("manifest.json", int64(len(manifestBytes)), bytes.NewReader(manifestBytes)); err != nil {
		return "", err
	}
	// 生成发布物 lock：按组件记录工件 SHA-256（相对路径，供安装校验发布物完整性）。
	lockEntries := make([]LockedArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		file, err := os.Open(artifact.path)
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return "", err
		}
		digest, err := hashReader(file)
		if err != nil {
			file.Close()
			return "", err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return "", err
		}
		if err := writeEntry(filepath.Base(artifact.path), info.Size(), file); err != nil {
			file.Close()
			return "", err
		}
		file.Close()
		lockEntries = append(lockEntries, LockedArtifact{
			ComponentID: artifact.componentID,
			Path:        filepath.Base(artifact.path),
			SHA256:      digest,
		})
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	lockBytes, err := json.Marshal(Lock{
		SchemaVersion: SchemaVersion, PackageID: manifest.ID,
		PackageVersion: manifest.Version,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts:      lockEntries,
	})
	if err != nil {
		return "", err
	}
	if err := writeEntry("lock.json", int64(len(lockBytes)), bytes.NewReader(lockBytes)); err != nil {
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

// hashReader 计算流式 SHA-256（与 HashFile 相同的摘要算法，避免双读文件）。
func hashReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
	destination, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer destination.Close()
	tarReader := tar.NewReader(gzipReader)
	var total int64
	entries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ErrInvalidFormat
		}
		entries++
		if entries > 4096 || !isPackageEntrypoint(header.Name) {
			return fmt.Errorf("%w: tarball 条目路径非法 %q", ErrInvalidFormat, header.Name)
		}
		if header.Size < 0 || header.Size > MaxArtifactBytes {
			return ErrInvalidFormat
		}
		total += header.Size
		if total > MaxArtifactBytes {
			return ErrInvalidFormat // 解压总量上限，防解压炸弹
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := destination.MkdirAll(header.Name, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := destination.MkdirAll(filepath.Dir(header.Name), 0o750); err != nil {
				return err
			}
			file, err := destination.OpenFile(header.Name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
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
