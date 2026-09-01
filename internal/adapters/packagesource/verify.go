package packagesource

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// VerifyHostedProtocol 在自动打包前用受限 WasmHost 逐组件执行一次协议探测。
// 自动提取只能知道 capability 名称，不能替 guest 生成分发器；没有合法 WASI
// 结果信封的工件直接拒绝，避免把安装后必然不可调用的能力发布出去。
func VerifyHostedProtocol(ctx context.Context, sourceDir string, manifest packagecontract.Manifest) error {
	for _, component := range manifest.Components {
		if component.Mode != packagecontract.ModeHosted {
			continue
		}
		artifactPath := filepath.Join(sourceDir, component.Entrypoint)
		host, err := loader.NewWasmHost(loader.WasmHostConfig{
			ReadArtifact: func(context.Context, loader.Manifest) ([]byte, error) {
				return packageio.ReadFileLimited(artifactPath, packagecontract.MaxArtifactBytes)
			},
		})
		if err != nil {
			return fmt.Errorf("packagesource: 构造协议校验宿主失败: %w", err)
		}
		runtime, err := host.Load(ctx, loader.Manifest{
			ID: component.ID, Version: manifest.Version, Mode: loader.ModeHosted,
		})
		if err != nil {
			return fmt.Errorf("packagesource: 组件 %s 装载失败: %w", component.ID, err)
		}
		stopRuntime := func() error {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			err := runtime.Stop(stopCtx)
			cancel()
			return err
		}
		invoker, ok := runtime.(loader.Invoker)
		if !ok {
			if err := stopRuntime(); err != nil {
				return fmt.Errorf("packagesource: 组件 %s 调用面校验清理失败: %w", component.ID, err)
			}
			return fmt.Errorf("packagesource: 组件 %s 不提供 Capability 调用面", component.ID)
		}
		for _, capabilityID := range component.Exports {
			_, invokeErr := invoker.Invoke(ctx, contracts.RequestContext{
				AppID: manifest.ID, EchoID: "package-verify", RequestID: capabilityID,
				ToolID: capabilityID, Deadline: time.Now().UTC().Add(30 * time.Second),
			}, []byte(`{}`))
			if invokeErr != nil && !errors.Is(invokeErr, loader.ErrHostedCallRejected) {
				stopErr := stopRuntime()
				if stopErr != nil {
					return fmt.Errorf("packagesource: 组件 %s 协议校验失败且清理失败: %w", component.ID, errors.Join(invokeErr, stopErr))
				}
				return fmt.Errorf("packagesource: 组件 %s 未返回可接受的 WASI 信封: %w", component.ID, invokeErr)
			}
		}
		stopErr := stopRuntime()
		if stopErr != nil {
			return fmt.Errorf("packagesource: 组件 %s 协议校验清理失败: %w", component.ID, stopErr)
		}
	}
	return nil
}
