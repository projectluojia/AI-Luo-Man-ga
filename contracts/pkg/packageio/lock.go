package packageio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// CanonicalLockDigest 计算安装 lock 的路径无关摘要。安装 lock 为了让宿主直接
// 执行而保存绝对路径，但项目 ailuo.lock 必须能在不同安装根复用，因此摘要把
// 工件、进程路径和包内 Unix socket 地址归一化为包目录相对路径。
func CanonicalLockDigest(ctx context.Context, directory string, lock packagecontract.Lock) (string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	canonical := lock
	canonical.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
	for index := range canonical.Artifacts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		artifact := &canonical.Artifacts[index]
		artifact.Path, err = relativePath(root, artifact.Path, false)
		if err != nil {
			return "", err
		}
		if artifact.Process != nil {
			process := *artifact.Process
			process.Path, err = relativePath(root, process.Path, false)
			if err != nil {
				return "", err
			}
			process.WorkDir, err = relativePath(root, process.WorkDir, true)
			if err != nil {
				return "", err
			}
			process.Address = canonicalAddress(root, process.Address)
			artifact.Process = &process
		}
	}
	sort.Slice(canonical.Artifacts, func(i, j int) bool {
		return canonical.Artifacts[i].ComponentID < canonical.Artifacts[j].ComponentID
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalAddress(root, address string) string {
	if !strings.HasPrefix(address, "unix:") {
		return address
	}
	socketPath := strings.TrimPrefix(address, "unix:")
	relative, err := filepath.Rel(root, socketPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return address
	}
	return "unix:" + filepath.ToSlash(relative)
}

func relativePath(root, value string, allowRoot bool) (string, error) {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", packagecontract.ErrInvalidFormat
	}
	relative, err := filepath.Rel(root, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		(relative == "." && !allowRoot) {
		return "", packagecontract.ErrInvalidFormat
	}
	return filepath.ToSlash(relative), nil
}
