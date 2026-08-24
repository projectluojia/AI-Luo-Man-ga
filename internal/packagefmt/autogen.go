package packagefmt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/packagefmt/schemaextract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

// packageIDPattern 与内核 id.StableLower 一致：自动生成清单的包 id 闭式格式。
var packageIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// AutoExtract 从包目录源码自动提取 capabilities：约定入口文件
// main.go（Go，go/ast）或 main.ts（TypeScript，ts-json-schema-generator）。
// 包 id 取目录名。
func AutoExtract(ctx context.Context, sourceDir string) ([]schemaextract.Capability, error) {
	absolute, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, err
	}
	id := filepath.Base(absolute)
	if source, err := os.ReadFile(filepath.Join(sourceDir, "main.go")); err == nil {
		return schemaextract.AnalyzeGo(source, id)
	}
	if source, err := os.ReadFile(filepath.Join(sourceDir, "main.ts")); err == nil {
		return schemaextract.AnalyzeTS(ctx, source, id, sourceDir)
	}
	return nil, fmt.Errorf("packagefmt: %q 未找到 main.go 或 main.ts", sourceDir)
}

// ManifestFromCapabilities 从源码提取的 capabilities 自动生成纯计算包清单
// （ailuo pack 在无 ailuo.toml 时使用）：作者只写 guest 源码，component/build/
// capability 全部自动推导。工具 schema 来自提取器，side_effect 统一 read
// （纯计算无副作用；需要写/宿主能力的包仍用 ailuo.toml 显式声明）。
func ManifestFromCapabilities(packageID string, capabilities []schemaextract.Capability) (packmgr.Manifest, []byte, error) {
	if !packageIDPattern.MatchString(packageID) {
		return packmgr.Manifest{}, nil, fmt.Errorf("packagefmt: 包 id %q 不合法", packageID)
	}
	if len(capabilities) == 0 {
		return packmgr.Manifest{}, nil, fmt.Errorf("packagefmt: 源码未提取到任何 capability")
	}
	source := sourceManifest{
		Package: sourcePackage{ID: packageID, Version: "0.1.0"},
		Tools:   make(map[string]sourceTool, len(capabilities)),
	}
	exportIDs := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		source.Tools[capability.ID] = sourceTool{
			Description: capability.Description,
			Schema:      string(capability.InputSchema),
			SideEffect:  registry.SideEffectRead,
		}
		source.Capabilities = append(source.Capabilities, sourceCapability{
			ID: capability.ID, Tool: capability.ID,
			Description: capability.Description,
		})
		exportIDs = append(exportIDs, capability.ID)
	}
	sort.Strings(exportIDs)
	extensions, err := source.buildExtensions()
	if err != nil {
		return packmgr.Manifest{}, nil, err
	}
	manifest := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion,
		ID:            packageID,
		Version:       "0.1.0",
		Components: []packmgr.Component{{
			ID: "main", Mode: "hosted", Entrypoint: "main.wasm", Exports: exportIDs,
		}},
		Extensions: extensions,
	}
	if err := packmgr.ValidateManifest(manifest); err != nil {
		return packmgr.Manifest{}, nil, fmt.Errorf("packagefmt: 自动生成清单校验失败: %w", err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return packmgr.Manifest{}, nil, err
	}
	return manifest, manifestBytes, nil
}
