package packmgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 以 GitHub Releases 作为包分发后端（REST，仅标准库）：发布走 tag+Release+
// asset（版本不可变，同版本重复发布报错）；安装走 Release 解析 + tarball
// 下载。token 来自 GITHUB_TOKEN/GH_TOKEN（发布与私有仓库需要，公开安装可缺省）。

const (
	githubAPI     = "https://api.github.com"
	githubUploads = "https://uploads.github.com"
)

// GitHubClient 是 GitHub Releases 分发后端客户端。
type GitHubClient struct {
	Token      string
	HTTP       *http.Client
	APIBase    string // 默认 githubAPI，测试注入 httptest 地址
	UploadBase string // 默认 githubUploads
}

// NewGitHubClient 从环境变量读取 token；公开安装无需 token。
func NewGitHubClient() *GitHubClient {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	return &GitHubClient{
		Token: token, HTTP: &http.Client{Timeout: 60 * time.Second},
		APIBase: githubAPI, UploadBase: githubUploads,
	}
}

// Publish 打包并发布到 GitHub Release：API 自动从默认分支创建 tag
// v{version}（无 git CLI 依赖），创建 Release 并上传 tarball。同版本
// 重复发布返回错误（版本不可变）。清单来自源目录 manifest.json。
func (c *GitHubClient) Publish(ctx context.Context, owner, repo, sourceDir string) (string, error) {
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return "", err
	}
	return c.PublishFromSource(ctx, owner, repo, sourceDir, source.Manifest, source.manifestBytes)
}

// PublishFromSource 用调用方提供的清单发布（与 PackFromSource 对称）：供作者侧
// 源清单（ailuo.toml）路径使用，不读取 manifest.json。
func (c *GitHubClient) PublishFromSource(ctx context.Context, owner, repo, sourceDir string, manifest Manifest, manifestBytes []byte) (string, error) {
	if owner == "" || repo == "" {
		return "", fmt.Errorf("发布需要 --repo owner/repo")
	}
	if c.Token == "" {
		return "", fmt.Errorf("发布需要 GITHUB_TOKEN（或 GH_TOKEN）")
	}
	tempDir, err := os.MkdirTemp("", "ailuo-publish-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	tarballPath, err := PackFromSource(ctx, sourceDir, tempDir, manifest, manifestBytes)
	if err != nil {
		return "", err
	}
	return c.publishTarball(ctx, owner, repo, tarballPath, manifest)
}

// PublishTarball 校验并发布已打包的 tarball。构建与发布分离时，发布端只需
// 处理已验证的发布物，不执行调用方提供的构建命令。
func (c *GitHubClient) PublishTarball(ctx context.Context, owner, repo, tarballPath string) (string, error) {
	if owner == "" || repo == "" {
		return "", fmt.Errorf("发布需要 --repo owner/repo")
	}
	if c.Token == "" {
		return "", fmt.Errorf("发布需要 GITHUB_TOKEN（或 GH_TOKEN）")
	}
	manifest, err := validateTarball(ctx, tarballPath)
	if err != nil {
		return "", fmt.Errorf("发布物校验失败: %w", err)
	}
	return c.publishTarball(ctx, owner, repo, tarballPath, manifest)
}

// validateTarball 校验发布物自身的清单、锁和工件，不把部署依赖当作发布前提。
// 依赖是否已安装属于目标 Deployment 的 Install 阶段，不应阻塞作者发布。
func validateTarball(ctx context.Context, tarballPath string) (Manifest, error) {
	sourceDir, cleanup, err := unpackSource(tarballPath)
	if err != nil {
		return Manifest{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := readSourceArtifacts(sourceDir, source.Manifest); err != nil {
		return Manifest{}, err
	}
	lockBytes, err := ReadFileLimited(filepath.Join(sourceDir, "lock.json"), MaxLockBytes)
	if err != nil {
		return Manifest{}, err
	}
	var lock Lock
	if err := DecodeStrictJSON(lockBytes, &lock); err != nil {
		return Manifest{}, err
	}
	manifestDigest := sha256.Sum256(source.manifestBytes)
	if lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return Manifest{}, ErrInvalidFormat
	}
	for index := range lock.Artifacts {
		if !isPackageEntrypoint(lock.Artifacts[index].Path) {
			return Manifest{}, ErrInvalidFormat
		}
		lock.Artifacts[index].Path = filepath.Join(sourceDir, lock.Artifacts[index].Path)
	}
	if err := validatePackagedLock(lock, source.Manifest); err != nil {
		return Manifest{}, err
	}
	for _, artifact := range lock.Artifacts {
		digest, err := HashFile(ctx, artifact.Path, MaxArtifactBytes)
		if err != nil || digest != artifact.SHA256 {
			return Manifest{}, ErrInvalidFormat
		}
	}
	return source.Manifest, nil
}

func validatePackagedLock(lock Lock, manifest Manifest) error {
	for _, artifact := range lock.Artifacts {
		if component, ok := findComponent(manifest, artifact.ComponentID); ok &&
			component.Mode == ModeIsolated && artifact.Process != nil {
			return ErrInvalidFormat
		}
	}
	packaged := manifest
	packaged.Components = append([]Component(nil), manifest.Components...)
	for index := range packaged.Components {
		if packaged.Components[index].Mode == ModeIsolated {
			packaged.Components[index].Mode = ModeHosted
		}
	}
	return ValidateLock(lock, packaged)
}

func (c *GitHubClient) publishTarball(ctx context.Context, owner, repo, tarballPath string, manifest Manifest) (string, error) {
	tag := "v" + manifest.Version
	release, err := c.createRelease(ctx, owner, repo, tag)
	if err != nil {
		return "", err
	}
	cleanupRelease := func(primary error) error {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if cleanupErr := c.deleteRelease(cleanupContext, owner, repo, release.ID); cleanupErr != nil {
			return errors.Join(primary, fmt.Errorf("清理失败的 Release: %w", cleanupErr))
		}
		return primary
	}
	assetName := manifest.ID + "-" + manifest.Version + ".tgz"
	assetURL := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s",
		c.UploadBase, owner, repo, release.ID, url.QueryEscape(assetName))
	file, err := os.Open(tarballPath)
	if err != nil {
		return "", cleanupRelease(err)
	}
	defer func() { _ = file.Close() }()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, assetURL, file)
	if err != nil {
		return "", cleanupRelease(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return "", cleanupRelease(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return "", cleanupRelease(fmt.Errorf("上传资产失败（HTTP %d）", response.StatusCode))
	}
	return release.HTMLURL, nil
}

func (c *GitHubClient) deleteRelease(ctx context.Context, owner, repo string, releaseID int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/repos/%s/%s/releases/%d", c.APIBase, owner, repo, releaseID), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("删除 Release 失败（HTTP %d）", response.StatusCode)
	}
	return nil
}

// gitHubRelease 是创建 Release 响应的最小结构。
type gitHubRelease struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// createRelease 创建 Release（tag 不存在时由 GitHub 自动从默认分支创建）。
func (c *GitHubClient) createRelease(ctx context.Context, owner, repo, tag string) (gitHubRelease, error) {
	payload, err := json.Marshal(map[string]string{
		"tag_name": tag, "name": tag, "body": "AI珞 包 " + tag,
	})
	if err != nil {
		return gitHubRelease{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.APIBase+"/repos/"+owner+"/"+repo+"/releases", bytes.NewReader(payload))
	if err != nil {
		return gitHubRelease{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return gitHubRelease{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnprocessableEntity {
		return gitHubRelease{}, fmt.Errorf("版本 %s 已发布（GitHub Release 不可变）", tag)
	}
	if response.StatusCode != http.StatusCreated {
		return gitHubRelease{}, fmt.Errorf("创建 Release 失败（HTTP %d）", response.StatusCode)
	}
	var release gitHubRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return gitHubRelease{}, err
	}
	return release, nil
}

// ResolveRelease 解析 owner/repo 的发行版：按 semver 约束选择最高版本，
// 返回版本号与 tarball 下载 URL。constraint 为空表示最新版。
func (c *GitHubClient) ResolveRelease(ctx context.Context, owner, repo, constraint string) (string, string, error) {
	var parsedConstraint Constraint
	if constraint != "" {
		var err error
		parsedConstraint, err = ParseConstraint(constraint)
		if err != nil {
			return "", "", err
		}
	}
	// GitHub /releases 按创建时间排序，不按版本：必须扫完所有页取最高版本，
	// 返回首个匹配项会在"旧版本线的补丁晚于新主版本发布"时解析出旧版本。
	var (
		bestVersion Version
		bestURL     string
		found       bool
	)
	for page := 1; ; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100&page=%d", c.APIBase, owner, repo, page), nil)
		if err != nil {
			return "", "", err
		}
		if c.Token != "" {
			request.Header.Set("Authorization", "Bearer "+c.Token)
		}
		response, err := c.HTTP.Do(request)
		if err != nil {
			return "", "", err
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			return "", "", fmt.Errorf("仓库 %s/%s 不存在或不可访问", owner, repo)
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return "", "", fmt.Errorf("查询 Release 失败（HTTP %d）", response.StatusCode)
		}
		var releases []struct {
			TagName string `json:"tag_name"`
			Assets  []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
			_ = response.Body.Close()
			return "", "", err
		}
		_ = response.Body.Close()
		if len(releases) == 0 {
			break // 空页之后不会再有 Release
		}
		for _, release := range releases {
			version, err := ParseVersion(strings.TrimPrefix(release.TagName, "v"))
			if err != nil {
				continue // 非 semver 的 tag（如 release-2026）跳过
			}
			if constraint != "" && !parsedConstraint.Matches(version) {
				continue
			}
			if found && CompareVersions(version, bestVersion) <= 0 {
				continue
			}
			for _, asset := range release.Assets {
				if strings.HasSuffix(asset.Name, ".tgz") {
					bestVersion, bestURL, found = version, asset.BrowserDownloadURL, true
					break
				}
			}
		}
	}
	if found {
		return bestVersion.String(), bestURL, nil
	}
	return "", "", fmt.Errorf("仓库 %s/%s 没有满足约束 %q 的发布包", owner, repo, constraint)
}

// DownloadRelease 下载发行版 tarball 到目标路径（大小受限）。
func (c *GitHubClient) DownloadRelease(ctx context.Context, assetURL, dest string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载资产失败（HTTP %d）", response.StatusCode)
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	written, err := io.Copy(file, io.LimitReader(response.Body, MaxArtifactBytes+1))
	if err != nil {
		return err
	}
	if written > MaxArtifactBytes {
		return ErrInvalidFormat
	}
	return nil
}

// InstallFromRelease 从 GitHub Release 解析、下载并安装包到安装根目录。
func InstallFromRelease(ctx context.Context, root string, client *GitHubClient, owner, repo, constraint string) (InstalledRecord, error) {
	version, assetURL, err := client.ResolveRelease(ctx, owner, repo, constraint)
	if err != nil {
		return InstalledRecord{}, err
	}
	tempDir, err := os.MkdirTemp("", "ailuo-registry-")
	if err != nil {
		return InstalledRecord{}, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	tarball := filepath.Join(tempDir, "package.tgz")
	if err := client.DownloadRelease(ctx, assetURL, tarball); err != nil {
		return InstalledRecord{}, err
	}
	sourceDir, cleanup, err := unpackSource(tarball)
	if err != nil {
		return InstalledRecord{}, err
	}
	defer cleanup()
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	resolved, err := ParseVersion(version)
	if err != nil {
		return InstalledRecord{}, err
	}
	manifestVersion, err := ParseVersion(source.Manifest.Version)
	if err != nil || CompareVersions(resolved, manifestVersion) != 0 {
		return InstalledRecord{}, fmt.Errorf("发布包版本 %s 与解析版本 %s 不一致", source.Manifest.Version, version)
	}
	record, err := Install(ctx, root, sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	return record, nil
}
