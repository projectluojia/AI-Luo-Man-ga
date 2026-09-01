package packagefmt

import (
	"context"
	"fmt"
	"os"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// Resolve 解析一个作者侧包目录。包必须提供 ailuo.toml；构建计划由清单显式声明，
// 不根据源码名称或目录内容猜测包身份与能力。
func Resolve(ctx context.Context, sourceDir string) (packagecontract.Manifest, []byte, error) {
	path := SourcePath(sourceDir)
	if _, err := os.Stat(path); err != nil {
		return packagecontract.Manifest{}, nil, fmt.Errorf("%w: 包必须提供 %s: %v", ErrSourceInvalid, SourceFileName, err)
	}
	manifest, manifestBytes, builds, err := Parse(path)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if err := Build(ctx, sourceDir, manifest, builds); err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	return manifest, manifestBytes, nil
}
