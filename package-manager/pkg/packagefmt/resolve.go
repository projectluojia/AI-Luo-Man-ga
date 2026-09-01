package packagefmt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// Resolve 解析一个作者侧包目录：显式 ailuo.toml 优先；没有源清单时只允许
// 通过 version 对零声明源码生成版本化清单。声明的 component 构建步骤在返回前完成。
func Resolve(ctx context.Context, sourceDir, version string) (packagecontract.Manifest, []byte, error) {
	path := SourcePath(sourceDir)
	_, statErr := os.Stat(path)
	if statErr == nil {
		manifest, manifestBytes, builds, err := Parse(path)
		if err != nil {
			return packagecontract.Manifest{}, nil, err
		}
		if err := Build(ctx, sourceDir, manifest, builds); err != nil {
			return packagecontract.Manifest{}, nil, err
		}
		return manifest, manifestBytes, nil
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return packagecontract.Manifest{}, nil, fmt.Errorf("%w: 读取源清单失败: %v", ErrSourceInvalid, statErr)
	}
	if version == "" {
		return packagecontract.Manifest{}, nil, fmt.Errorf("%w: 零声明包必须提供版本（CLI 使用 --version）", ErrSourceInvalid)
	}
	capabilities, buildTool, err := AutoExtract(ctx, sourceDir)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	absolute, err := filepath.Abs(sourceDir)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	manifest, manifestBytes, err := ManifestFromCapabilities(filepath.Base(absolute), version, capabilities)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if err := Build(ctx, sourceDir, manifest, []BuildSpec{{Tool: buildTool}}); err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	return manifest, manifestBytes, nil
}
