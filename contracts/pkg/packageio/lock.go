package packageio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// CanonicalLockDigest 计算安装 lock 的路径无关摘要。安装 lock 为了让宿主直接
// 执行而保存绝对路径，但项目 ailuo.lock 必须能在不同安装根复用，因此摘要把
// 工件、进程路径和包内 Unix socket 地址归一化为包目录相对路径；外部 Unix
// 地址保留显式值。
func CanonicalLockDigest(ctx context.Context, directory string, lock packagecontract.Lock) (string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	canonical := lock
	canonical.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
	seenComponents := make(map[string]struct{}, len(canonical.Artifacts))
	for index := range canonical.Artifacts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		artifact := &canonical.Artifacts[index]
		if _, exists := seenComponents[artifact.ComponentID]; exists {
			return "", fmt.Errorf("%w: duplicate component id %q", packagecontract.ErrInvalidFormat, artifact.ComponentID)
		}
		seenComponents[artifact.ComponentID] = struct{}{}
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
			address, err := canonicalAddress(root, process.Address)
			if err != nil {
				return "", err
			}
			if address != process.Address && process.Args != nil {
				process.Args = append([]string(nil), process.Args...)
				for index, argument := range process.Args {
					process.Args[index] = strings.ReplaceAll(argument, process.Address, address)
				}
			}
			process.Address = address
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

func canonicalAddress(root, address string) (string, error) {
	if !packagecontract.IsLocalRuntimeAddress(address) {
		return "", packagecontract.ErrInvalidFormat
	}
	if !strings.HasPrefix(address, "unix:") {
		return address, nil
	}
	socketPath := strings.TrimPrefix(address, "unix:")
	relative, err := filepath.Rel(root, socketPath)
	if err != nil {
		// 不同盘符/挂载点的 Unix 地址属于外部运行时，保留其显式稳定值。
		return address, nil //nolint:nilerr // filepath.Rel 无法跨卷计算相对路径
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		// 安装根外的绝对 Unix 地址是外部 executor 的稳定地址，不随安装根归一化。
		return address, nil
	}
	return "unix:" + filepath.ToSlash(relative), nil
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
