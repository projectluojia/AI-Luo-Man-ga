package packagefmt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// Resolve 解析一个显式声明的作者侧包目录，并在返回前完成声明的构建步骤。
func Resolve(ctx context.Context, sourceDir string) (packagecontract.Manifest, []byte, error) {
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
	return packagecontract.Manifest{}, nil, fmt.Errorf("%w: 缺少 ailuo.toml，必须显式声明 Package、Component 和 Capability", ErrSourceInvalid)
}
