package packmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Install 从源包目录安装到安装根目录：校验源清单（manifest.json + entrypoint
// 工件）、解析依赖（已安装包满足约束）、原子发布 manifest+lock+artifact，并
// 回读验证。目标已有同 ID 且版本不同时替换（升级语义）；版本相同视为重复
// 安装并返回错误。
func Install(ctx context.Context, root, sourceDir string) (InstalledRecord, error) {
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return InstalledRecord{}, err
	}
	if err := resolveDependencies(ctx, root, source.Manifest.Dependencies); err != nil {
		return InstalledRecord{}, err
	}
	targetDir := filepath.Join(root, source.Manifest.ID)
	if existing, err := ReadInstalled(ctx, targetDir); err == nil &&
		existing.Manifest.Version == source.Manifest.Version {
		return InstalledRecord{}, fmt.Errorf("包 %s@%s 已安装", source.Manifest.ID, source.Manifest.Version)
	}
	stageDir, err := createStageDir(root)
	if err != nil {
		return InstalledRecord{}, err
	}
	defer os.RemoveAll(stageDir)
	// 写入 manifest（保留源字节，lock 中的 manifest digest 必须一致）。
	if err := os.WriteFile(filepath.Join(stageDir, "manifest.json"), source.manifestBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	// 写入工件并计算哈希。
	artifactName := filepath.Base(source.Manifest.Entrypoint)
	stageArtifact := filepath.Join(stageDir, artifactName)
	if err := copyFile(source.artifactPath, stageArtifact); err != nil {
		return InstalledRecord{}, err
	}
	artifactDigest, err := HashFile(ctx, stageArtifact, MaxArtifactBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	// 写入 lock：ArtifactPath 引用发布后的最终路径（stage 目录会被 rename 为目标）。
	manifestDigest := sha256.Sum256(source.manifestBytes)
	targetArtifact := filepath.Join(targetDir, artifactName)
	lockBytes, err := json.Marshal(Lock{
		SchemaVersion: SchemaVersion, PackageID: source.Manifest.ID,
		PackageVersion: source.Manifest.Version, Mode: source.Manifest.Mode,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		ArtifactSHA256: artifactDigest,
		ArtifactPath:   targetArtifact,
	})
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "lock.json"), lockBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	// 原子发布：目标先移除再 rename（Windows 不支持 rename 覆盖目录）。
	if err := os.RemoveAll(targetDir); err != nil {
		return InstalledRecord{}, err
	}
	if err := os.Rename(stageDir, targetDir); err != nil {
		return InstalledRecord{}, err
	}
	record, err := ReadInstalled(ctx, targetDir)
	if err != nil {
		return InstalledRecord{}, fmt.Errorf("安装后验证失败: %w", err)
	}
	return record, nil
}

// Upgrade 要求包已安装且源目录版本号不同，然后安装。
func Upgrade(ctx context.Context, root, id, sourceDir string) (InstalledRecord, error) {
	existing, err := ReadInstalled(ctx, filepath.Join(root, id))
	if err != nil {
		return InstalledRecord{}, fmt.Errorf("包 %q 未安装", id)
	}
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if source.Manifest.ID != id {
		return InstalledRecord{}, fmt.Errorf("源目录包 ID %q 与升级目标 %q 不一致", source.Manifest.ID, id)
	}
	if source.Manifest.Version == existing.Manifest.Version {
		return InstalledRecord{}, fmt.Errorf("包 %s 已安装版本 %s，升级目标版本相同", id, existing.Manifest.Version)
	}
	return Install(ctx, root, sourceDir)
}

// Uninstall 删除已安装包目录。仅当目录包含 manifest.json 时删除（安全防护）。
func Uninstall(_ context.Context, root, id string) error {
	target := filepath.Join(root, id)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("包 %q 未安装", id)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("删除失败: %w", err)
	}
	manifestInfo, err := os.Lstat(filepath.Join(target, "manifest.json"))
	if err != nil || !manifestInfo.Mode().IsRegular() {
		return fmt.Errorf("目录 %q 不是安装包（缺少 manifest.json）", target)
	}
	return os.RemoveAll(target)
}

// sourcePackage 是源包目录读取结果：清单字节原样保留以锁定 digest。
type sourcePackage struct {
	Manifest      Manifest
	artifactPath  string
	manifestBytes []byte
}

// readSourceManifest 读取源包目录的 manifest.json 与 entrypoint 工件。
func readSourceManifest(sourceDir string) (sourcePackage, error) {
	manifestBytes, err := ReadFileLimited(filepath.Join(sourceDir, "manifest.json"), MaxManifestBytes)
	if err != nil {
		return sourcePackage{}, err
	}
	var manifest Manifest
	if err := DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		return sourcePackage{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return sourcePackage{}, err
	}
	if manifest.Entrypoint == "" {
		return sourcePackage{}, fmt.Errorf("清单缺少 entrypoint")
	}
	artifactPath := filepath.Join(sourceDir, manifest.Entrypoint)
	relative, err := filepath.Rel(sourceDir, artifactPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return sourcePackage{}, fmt.Errorf("entrypoint 超出源目录")
	}
	info, err := os.Lstat(artifactPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxArtifactBytes {
		return sourcePackage{}, fmt.Errorf("entrypoint 工件无效: %w", err)
	}
	return sourcePackage{
		Manifest: manifest, artifactPath: artifactPath, manifestBytes: manifestBytes,
	}, nil
}

// resolveDependencies 检查每个依赖在安装根内存在已安装包且版本满足约束。
func resolveDependencies(ctx context.Context, root string, deps []Dependency) error {
	if len(deps) == 0 {
		return nil
	}
	installed, err := ListInstalled(ctx, root)
	if err != nil {
		return err
	}
	versionByID := make(map[string]string, len(installed))
	for _, record := range installed {
		versionByID[record.Manifest.ID] = record.Manifest.Version
	}
	var missing []string
	for _, dep := range deps {
		versionText, ok := versionByID[dep.ID]
		if !ok {
			missing = append(missing, dep.ID+" (未安装)")
			continue
		}
		candidate, err := ParseVersion(versionText)
		if err != nil {
			return fmt.Errorf("已安装包 %s 版本 %q 非法: %w", dep.ID, versionText, err)
		}
		constraint, err := ParseConstraint(dep.Constraint)
		if err != nil {
			return fmt.Errorf("依赖约束 %s@%s 非法: %w", dep.ID, dep.Constraint, err)
		}
		if !constraint.Matches(candidate) {
			missing = append(missing, fmt.Sprintf("%s@%s (已安装 %s)", dep.ID, dep.Constraint, versionText))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少依赖: %s", strings.Join(missing, "; "))
	}
	return nil
}

// createStageDir 在安装根目录创建临时阶段目录。
func createStageDir(root string) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	path := filepath.Join(root, ".stage-"+hex.EncodeToString(random))
	if err := os.Mkdir(path, 0o750); err != nil {
		return "", err
	}
	return path, nil
}

// copyFile 复制文件并保留源权限位。
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	dest, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer dest.Close()
	if _, err := io.Copy(dest, source); err != nil {
		return err
	}
	return nil
}
