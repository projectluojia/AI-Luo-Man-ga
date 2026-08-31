package demo

import (
	"context"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	ecardtool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

// LoadECardData 启用珞珈 E 卡演示入口：不写入真实校园卡数据，也不播种可当作
// 生产使用的 CAS Cookie。演示凭据只能经 ecard.credentials.put 以 demo_handle
// 形式显式写入，且不能用于生产会话准备。
func LoadECardData(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = now
	observe.Warn(ctx, "已启用非权威珞珈 E 卡演示入口",
		observe.BoolAttr("authoritative", false),
		observe.StringAttr("source", ecardtool.DemoSource),
		observe.IntAttr("entry_count", 2),
	)
	return nil
}
