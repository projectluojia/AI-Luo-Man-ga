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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
)

const maxTarEntries = 4096

// PackFromSource 用调用方提供的清单（如从 ailuo.toml 源清单或 schemaextract
// 自动提取）打包：校验清单与源目录工件，生成 tarball 并附带按工件 SHA-256
// 锁定的 lock.json。清单来源由调用方决定，本函数不读取 manifest.json。
func PackFromSource(ctx context.Context, sourceDir, outputDir string, manifest packagecontract.Manifest, manifestBytes []byte) (string, error) {
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		return "", err
	}
	artifacts, err := readSourceArtifacts(ctx, sourceDir, manifest)
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
	// 失败路径关掉描述符即可（tarball 不完整，调用方不会使用）；成功路径在末尾
	// 显式 Sync + Close，ENOSPC 这类错误只在 flush/close 时才浮出来。
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	writeFileEntry := func(name string, info os.FileInfo, body io.Reader) error {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(tarWriter, body)
		return err
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o640, Size: int64(len(manifestBytes)), Typeflag: tar.TypeReg}); err != nil {
		return "", err
	}
	if _, err := io.Copy(tarWriter, bytes.NewReader(manifestBytes)); err != nil {
		return "", err
	}
	// 生成发布物 lock：按组件记录文件或目录工件 SHA-256（相对路径，供安装校验发布物完整性）。
	lockEntries := make([]packagecontract.LockedArtifact, 0, len(artifacts))
	archiveEntries := 2 // manifest.json 与末尾的 lock.json
	for _, artifact := range artifacts {
		info, err := os.Lstat(artifact.path)
		if err != nil {
			return "", err
		}
		digest, err := HashArtifact(ctx, artifact.path, packagecontract.MaxArtifactBytes)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			entries, err := writeArtifactEntries(tarWriter, artifact.path, filepath.Base(artifact.path), maxTarEntries-archiveEntries)
			if err != nil {
				return "", fmt.Errorf("组件 %s 工件打包失败: %w", artifact.componentID, err)
			}
			archiveEntries += entries
		} else {
			if archiveEntries >= maxTarEntries {
				return "", packagecontract.ErrInvalidFormat
			}
			file, err := os.Open(artifact.path)
			if err != nil {
				return "", err
			}
			if err := writeFileEntry(filepath.Base(artifact.path), info, file); err != nil {
				_ = file.Close()
				return "", err
			}
			if err := file.Close(); err != nil {
				return "", err
			}
			archiveEntries++
		}
		lockEntries = append(lockEntries, packagecontract.LockedArtifact{
			ComponentID: artifact.componentID,
			Path:        filepath.Base(artifact.path),
			SHA256:      digest,
		})
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	lockBytes, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: manifest.ID,
		PackageVersion: manifest.Version,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts:      lockEntries,
	})
	if err != nil {
		return "", err
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "lock.json", Mode: 0o640, Size: int64(len(lockBytes)), Typeflag: tar.TypeReg}); err != nil {
		return "", err
	}
	if _, err := io.Copy(tarWriter, bytes.NewReader(lockBytes)); err != nil {
		return "", err
	}
	// tar 尾 + gzip 尾 + 文件按序关闭；截断只在 flush/close 时才报错，必须检查。
	if err := tarWriter.Close(); err != nil {
		return "", err
	}
	if err := gzipWriter.Close(); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	closed = true
	if err := file.Close(); err != nil {
		return "", err
	}
	return tarballPath, nil
}

// writeArtifactEntries 把目录工件按稳定的相对路径写入 tar；目录和普通文件
// 均保留必要的权限位，且不写入符号链接或特殊文件。
func writeArtifactEntries(writer *tar.Writer, sourceRoot, archiveRoot string, limit int) (int, error) {
	entries := 0
	err := filepath.WalkDir(sourceRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > limit {
			return fmt.Errorf("%w: 目录工件条目超过上限 %d", packagecontract.ErrInvalidFormat, limit)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: 目录工件包含符号链接", packagecontract.ErrInvalidFormat)
		}
		relative, err := filepath.Rel(sourceRoot, current)
		if err != nil {
			return packagecontract.ErrInvalidFormat
		}
		name := filepath.ToSlash(filepath.Join(archiveRoot, relative))
		if relative == "." {
			name = filepath.ToSlash(archiveRoot)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return writer.WriteHeader(&tar.Header{Name: name + "/", Mode: int64(info.Mode().Perm()), Typeflag: tar.TypeDir})
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: 目录工件包含非普通文件", packagecontract.ErrInvalidFormat)
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			_ = file.Close()
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	return entries, err
}

// unpackTarball 严格解压 tarball 到目标目录：拒绝绝对路径、路径穿越、
// 非普通文件/目录条目与超限大小（tarball 是外部输入，属于信任边界）。
func unpackTarball(tarball, dest string) error {
	file, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return packagecontract.ErrInvalidFormat
	}
	defer func() { _ = gzipReader.Close() }()
	destination, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer destination.Close()
	tarReader := tar.NewReader(gzipReader)
	var total int64
	entries := 0
	seen := make(map[string]struct{})
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return packagecontract.ErrInvalidFormat
		}
		entries++
		if entries > maxTarEntries {
			return fmt.Errorf("%w: tarball 条目数超过上限 %d", packagecontract.ErrInvalidFormat, maxTarEntries)
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			if !strings.HasSuffix(name, "/") || strings.HasSuffix(name, "//") {
				return fmt.Errorf("%w: tarball 目录条目路径非法 %q", packagecontract.ErrInvalidFormat, header.Name)
			}
			name = strings.TrimSuffix(name, "/")
		}
		if !packagecontract.IsPackagePath(name) || name == "." {
			return fmt.Errorf("%w: tarball 条目路径非法 %q", packagecontract.ErrInvalidFormat, header.Name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: tarball 条目重复 %q", packagecontract.ErrInvalidFormat, name)
		}
		seen[name] = struct{}{}
		if header.Size < 0 || header.Size > packagecontract.MaxArtifactBytes {
			return packagecontract.ErrInvalidFormat
		}
		total += header.Size
		if total > packagecontract.MaxArtifactBytes {
			return packagecontract.ErrInvalidFormat // 解压总量上限，防解压炸弹
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if name == "manifest.json" || name == "lock.json" || header.Size != 0 {
				return packagecontract.ErrInvalidFormat
			}
			if err := destination.MkdirAll(name, 0o750); err != nil {
				return err
			}
			if err := destination.Chmod(name, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := destination.MkdirAll(filepath.Dir(name), 0o750); err != nil {
				return err
			}
			file, err := destination.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			// tar.Reader 按条目边界供给，超量读不到；不足声明大小（截断）报错。
			if _, err := io.CopyN(file, tarReader, header.Size); err != nil {
				_ = file.Close()
				return packagecontract.ErrInvalidFormat
			}
			if err := file.Close(); err != nil {
				return err
			}
			if err := destination.Chmod(name, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tarball 包含不支持条目 %q", packagecontract.ErrInvalidFormat, header.Name)
		}
	}
}
