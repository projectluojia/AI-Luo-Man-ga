package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
)

// 内置 agent 的包身份：它是随内核一起发布的普通 executor 包，组合根知道
// 自己发布的包 ID 属正常部署知识；内核装载与解析路径对它没有任何特判。
const (
	builtinAgentPackageID      = "ailuo.agent"
	builtinAgentPackageVersion = "1.0.0"
	builtinAgentComponentID    = "agent"
)

// builtinAgentSource 是内置 agent 的虚拟包源：以与安装目录完全一致的
// 记录/解析/校验契约接入注册管线。它只是"我们随内核一起发布的包"的装载
// 便利，不赋予 agent 任何特殊身份——换成第三方 executor 包时走同一契约。
type builtinAgentSource struct {
	manifest   loader.Manifest
	pythonPath string
	address    string
	workDir    string
	spawn      bool
	model      string
	env        []string
}

// newBuiltinAgentSource 装配内置 agent 包源：digest 对 agent 源码树（含
// uv.lock）确定性计算——与安装包对工件求哈希同一性质，不再对常量字符串
// 求哈希伪装身份。
func newBuiltinAgentSource(cfg config, model string) (*builtinAgentSource, error) {
	digest, err := hashAgentSource(cfg.projectRoot)
	if err != nil {
		return nil, fmt.Errorf("hash built-in agent source: %w", err)
	}
	return &builtinAgentSource{
		manifest: loader.Manifest{
			ID: builtinAgentPackageID, Version: builtinAgentPackageVersion,
			Mode: loader.ModeIsolated, Role: loader.RoleExecutor,
			LockedDigest: digest, Pin: true,
		},
		pythonPath: cfg.pythonPath,
		address:    cfg.agentAddress,
		workDir:    cfg.projectRoot,
		spawn:      cfg.manageAgent,
		model:      model,
		env:        agentEnvironment(cfg),
	}, nil
}

// Record 返回内核注册管线使用的安装记录（executor 角色，无能力面）。
func (s *builtinAgentSource) Record() loader.InstalledRecord {
	return loader.InstalledRecord{
		Runtime:     s.manifest,
		PackageID:   builtinAgentPackageID,
		ComponentID: builtinAgentComponentID,
	}
}

// ResolveProcess 按清单返回进程规格；身份不符拒绝（宿主 Verify fail-closed）。
func (s *builtinAgentSource) ResolveProcess(_ context.Context, manifest loader.Manifest) (packagecontract.ProcessSpec, error) {
	if !manifest.Equal(s.manifest) {
		return packagecontract.ProcessSpec{}, packagesourceChanged
	}
	return s.processSpec(), nil
}

// VerifyProcess 复核清单与解析出的规格未被替换。
func (s *builtinAgentSource) VerifyProcess(_ context.Context, manifest loader.Manifest, spec packagecontract.ProcessSpec) error {
	if !manifest.Equal(s.manifest) || !reflect.DeepEqual(s.processSpec(), spec) {
		return packagesourceChanged
	}
	return nil
}

func (s *builtinAgentSource) processSpec() packagecontract.ProcessSpec {
	return packagecontract.ProcessSpec{
		Path: s.pythonPath, Args: []string{"-m", "agent.runtime", "--listen", s.address},
		Env: append([]string(nil), s.env...), WorkDir: s.workDir, Address: s.address,
		Limits: packagecontract.ProcessLimits{},
	}
}

var packagesourceChanged = errors.New("built-in agent changed after discovery")

// hashAgentSource 对 agent 源码树确定性求哈希：按斜杠路径排序，逐文件写入
// 路径与内容。排除虚拟环境、缓存与编译产物——digest 锁定"认知逻辑"本身。
func hashAgentSource(projectRoot string) (string, error) {
	agentRoot := filepath.Join(projectRoot, "agent")
	digest := sha256.New()
	var paths []string
	err := filepath.WalkDir(agentRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == ".venv" || name == "__pycache__" || name == ".pytest_cache" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(name) == ".pyc" || name == ".DS_Store" {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return "", fmt.Errorf("agent source tree %s is empty", agentRoot)
	}
	for _, path := range paths {
		relative, err := filepath.Rel(agentRoot, path)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		digest.Write([]byte(filepath.ToSlash(relative)))
		digest.Write([]byte{0})
		digest.Write(content)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// agentEnvironment 组合根为内置 agent 包解析声明的配置项（env_from 语义：
// 包声明名字，组合根从部署配置供给值；凭据永不进包本体）。
func agentEnvironment(config config) []string {
	return []string{
		"AILUO_ENVIRONMENT=" + config.environment,
		"AILUO_MODEL_API_KEY_FILE=" + config.modelAPIKeyFile,
		"AILUO_MODEL_BASE_URL=" + config.modelBaseURL,
		"AILUO_MODEL_TIMEOUT_SECONDS=" + strconv.FormatFloat(config.modelRequestTimeout.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_READINESS_TIMEOUT_SECONDS=" + strconv.FormatFloat(config.modelReadyTimeout.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_MAX_RETRIES=" + strconv.Itoa(config.modelMaxRetries),
		"AILUO_MODEL_RETRY_BASE_SECONDS=" + strconv.FormatFloat(config.modelRetryBase.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_RETRY_MAX_SECONDS=" + strconv.FormatFloat(config.modelRetryMax.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_REQUESTS_PER_MINUTE=" + strconv.Itoa(config.modelRequestsMinute),
		"AILUO_MODEL_MAX_CONCURRENCY=" + strconv.Itoa(config.modelMaxConcurrency),
	}
}
