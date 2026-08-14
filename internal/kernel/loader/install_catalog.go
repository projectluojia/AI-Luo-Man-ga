package loader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	InstallSchemaVersion = "ailuo.install.v2"
	installManifestName  = "manifest.json"
	installLockName      = "lock.json"
	maxInstalledRuntimes = 256
	maxInstallManifest   = 256 << 10
	maxInstallLock       = 64 << 10
	maxInstallArtifact   = int64(1 << 30)
	maxInstalledSpecs    = 4096
)

var (
	ErrInstallCatalogInvalid = errors.New("installed runtime catalog is invalid")
	ErrInstallChanged        = errors.New("installed runtime changed after discovery")
)

type InstalledRuntimeSpec struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	Pin       bool   `json:"pin"`
	IdleTTLMS uint64 `json:"idle_ttl_ms"`
}

type InstalledManifest struct {
	SchemaVersion string                    `json:"schema_version"`
	Runtime       InstalledRuntimeSpec      `json:"runtime"`
	Tools         []registry.ToolSpec       `json:"tools"`
	Service       registry.ServiceSpec      `json:"service"`
	Capabilities  []registry.CapabilitySpec `json:"capabilities"`
}

type InstalledProcessSpec struct {
	Path    string        `json:"path"`
	Args    []string      `json:"args"`
	Env     []string      `json:"env"`
	WorkDir string        `json:"work_dir"`
	Address string        `json:"address"`
	Limits  ProcessLimits `json:"limits,omitempty"`
}

type InstalledLock struct {
	SchemaVersion  string                `json:"schema_version"`
	RuntimeID      string                `json:"runtime_id"`
	RuntimeVersion string                `json:"runtime_version"`
	Mode           string                `json:"mode"`
	ManifestSHA256 string                `json:"manifest_sha256"`
	ArtifactSHA256 string                `json:"artifact_sha256"`
	ArtifactPath   string                `json:"artifact_path"`
	Process        *InstalledProcessSpec `json:"process,omitempty"`
}

type InstalledRecord struct {
	Directory    string
	ArtifactPath string
	Runtime      Manifest
	Tools        []registry.ToolSpec
	Service      registry.ServiceSpec
	Capabilities []registry.CapabilitySpec
	Process      *ProcessSpec
}

type Catalog struct {
	root string
}

func NewCatalog(root string) (*Catalog, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInstallCatalogInvalid
	}
	return &Catalog{root: root}, nil
}

func (c *Catalog) Discover(ctx context.Context) ([]InstalledRecord, error) {
	if c == nil || c.root == "" {
		return nil, ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	if len(entries) > maxInstalledRuntimes {
		return nil, ErrInstallCatalogInvalid
	}
	records := make([]InstalledRecord, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil, ErrInstallCatalogInvalid
		}
		directory := filepath.Join(c.root, entry.Name())
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
			return nil, ErrInstallCatalogInvalid
		}
		record, err := c.readRecord(ctx, directory)
		if err != nil {
			return nil, err
		}
		if entry.Name() != record.Runtime.ID {
			return nil, ErrInstallCatalogInvalid
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Runtime.ID < records[j].Runtime.ID })
	if err := validateInstalledRecords(records); err != nil {
		return nil, err
	}
	return records, nil
}

func (c *Catalog) VerifyRuntime(ctx context.Context, manifest Manifest) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) {
		return ErrInstallChanged
	}
	return nil
}

func (c *Catalog) ResolveProcess(ctx context.Context, manifest Manifest) (ProcessSpec, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return ProcessSpec{}, err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) || record.Process == nil {
		return ProcessSpec{}, ErrInstallChanged
	}
	return cloneProcessSpec(*record.Process), nil
}

func (c *Catalog) VerifyProcess(ctx context.Context, manifest Manifest, process ProcessSpec) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) || record.Process == nil ||
		!reflect.DeepEqual(cloneProcessSpec(*record.Process), cloneProcessSpec(process)) {
		return ErrInstallChanged
	}
	return nil
}

// ReadArtifact 读取与 manifest 锁定的 hosted 工件字节。
// 每次读取都重新校验目录、属主、清单一致性、工件 digest 与大小，防止 TOCTOU 替换。
// 与 manifest 的比较只限定身份字段（ID/Version/Mode）：工件 digest 已在 readRecord
// 内重新校验，Role/Pin/IdleTTL 不参与工件装载；且外部 Runtime Host 协议身份只携带
// ID/Version，携带完整清单字段的绑定校验由内核侧 VerifyRuntime 负责。
func (c *Catalog) ReadArtifact(ctx context.Context, manifest Manifest) ([]byte, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return nil, err
	}
	if !sameRuntimeIdentity(record.Runtime, manifest) {
		return nil, ErrInstallChanged
	}
	if record.Runtime.Mode != ModeHosted {
		return nil, ErrUnsupportedMode
	}
	data, err := os.ReadFile(record.ArtifactPath)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	if int64(len(data)) > maxInstallArtifact {
		return nil, ErrInstallCatalogInvalid
	}
	return data, nil
}

func (c *Catalog) readRecordByID(ctx context.Context, id string) (InstalledRecord, error) {
	if c == nil || !stableIDPattern.MatchString(id) {
		return InstalledRecord{}, ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	directory := filepath.Join(c.root, id)
	if filepath.Dir(directory) != c.root {
		return InstalledRecord{}, ErrInstallCatalogInvalid
	}
	return c.readRecord(ctx, directory)
}

func (c *Catalog) readRecord(ctx context.Context, directory string) (InstalledRecord, error) {
	if err := validateSecureDirectory(directory); err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	manifestBytes, err := readSecureJSONFile(filepath.Join(directory, installManifestName), maxInstallManifest)
	if err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	lockBytes, err := readSecureJSONFile(filepath.Join(directory, installLockName), maxInstallLock)
	if err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	var installed InstalledManifest
	if err := decodeStrictJSON(manifestBytes, &installed); err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	var lock InstalledLock
	if err := decodeStrictJSON(lockBytes, &lock); err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	if installed.SchemaVersion != InstallSchemaVersion || lock.SchemaVersion != InstallSchemaVersion ||
		lock.RuntimeID != installed.Runtime.ID || lock.RuntimeVersion != installed.Runtime.Version ||
		lock.Mode != installed.Runtime.Mode ||
		lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return InstalledRecord{}, ErrInstallCatalogInvalid
	}
	if installed.Runtime.Mode != ModeHosted && installed.Runtime.Mode != ModeIsolated {
		return InstalledRecord{}, ErrUnsupportedMode
	}
	if installed.Runtime.IdleTTLMS > uint64((30*24*time.Hour)/time.Millisecond) {
		return InstalledRecord{}, ErrInstallCatalogInvalid
	}
	// installed 包一律是能力提供者角色：其线协议（runtime_host.proto）是
	// 请求/响应的能力执行协议；执行者角色需要执行者会话协议，由未来
	// 专门的宿主承载，不允许以 installed 清单伪装。
	runtimeManifest := Manifest{
		ID: installed.Runtime.ID, Version: installed.Runtime.Version, Mode: installed.Runtime.Mode,
		Role: RoleCapability, LockedDigest: lock.ArtifactSHA256, Pin: installed.Runtime.Pin,
		IdleTTL: time.Duration(installed.Runtime.IdleTTLMS) * time.Millisecond,
	}
	if err := validateManifest(runtimeManifest); err != nil {
		return InstalledRecord{}, err
	}
	artifactPath, err := validateInstalledPath(directory, lock.ArtifactPath, false)
	if err != nil {
		return InstalledRecord{}, err
	}
	artifactDigest, err := hashInstalledArtifact(ctx, artifactPath)
	if err != nil || artifactDigest != lock.ArtifactSHA256 {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	var process *ProcessSpec
	switch installed.Runtime.Mode {
	case ModeHosted:
		if lock.Process != nil {
			return InstalledRecord{}, ErrInstallCatalogInvalid
		}
	case ModeIsolated:
		if lock.Process == nil || lock.Process.Path != artifactPath {
			return InstalledRecord{}, ErrInstallCatalogInvalid
		}
		workDir, err := validateInstalledPath(directory, lock.Process.WorkDir, true)
		if err != nil {
			return InstalledRecord{}, err
		}
		spec := ProcessSpec{
			Path: artifactPath, Args: append([]string(nil), lock.Process.Args...),
			Env: append([]string(nil), lock.Process.Env...), WorkDir: workDir, Address: lock.Process.Address,
			Limits: lock.Process.Limits,
		}
		if err := validateProcessSpec(spec); err != nil {
			return InstalledRecord{}, err
		}
		process = &spec
	}
	record := InstalledRecord{
		Directory: directory, ArtifactPath: artifactPath, Runtime: runtimeManifest,
		Tools: cloneToolSpecs(installed.Tools), Service: cloneInstalledService(installed.Service),
		Capabilities: cloneCapabilitySpecs(installed.Capabilities), Process: process,
	}
	if err := validateInstalledRecord(record); err != nil {
		return InstalledRecord{}, err
	}
	return record, nil
}

func RegisterInstalled(ctx context.Context, manager *Manager, target *registry.Registry, records []InstalledRecord) error {
	if manager == nil || target == nil || len(records) == 0 || len(records) > maxInstalledRuntimes {
		return ErrInstallCatalogInvalid
	}
	manifests := make([]Manifest, 0, len(records))
	tools := make([]registry.ToolRegistration, 0)
	services := make([]registry.ServiceRegistration, 0, len(records))
	for _, record := range records {
		manifests = append(manifests, record.Runtime)
		handler := manager.Handler(record.Runtime.ID)
		for _, spec := range record.Tools {
			tools = append(tools, registry.ToolRegistration{Spec: spec, Handler: handler})
		}
		capabilities := make(map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}, len(record.Capabilities))
		for _, spec := range record.Capabilities {
			capabilities[spec.ID] = struct {
				Spec    registry.CapabilitySpec
				Handler registry.Handler
			}{Spec: spec, Handler: handler}
		}
		// 运行时专用记录（如内置 agent：Service 依赖内核 Orchestrator，装配完成后
		// 单独注册）不携带 Service 规格，只注册运行时清单。
		if record.Service.ID != "" {
			services = append(services, registry.ServiceRegistration{
				Spec: record.Service, Capabilities: capabilities,
			})
		}
	}
	if err := manager.RegisterBatch(ctx, manifests); err != nil {
		return err
	}
	if len(tools) == 0 && len(services) == 0 {
		return nil // 记录只声明运行时清单（如内置 agent），无 Registry 规格
	}
	if err := target.RegisterBatch(tools, services); err != nil {
		return errors.Join(err, manager.rollbackRegistered(manifests))
	}
	return nil
}

func validateInstalledRecords(records []InstalledRecord) error {
	if len(records) == 0 {
		return nil
	}
	totalSpecs := 0
	seenRuntimes := make(map[string]struct{}, len(records))
	for _, record := range records {
		if _, duplicate := seenRuntimes[record.Runtime.ID]; duplicate {
			return ErrDuplicateID
		}
		seenRuntimes[record.Runtime.ID] = struct{}{}
		totalSpecs += len(record.Tools) + len(record.Capabilities) + 1
		if totalSpecs > maxInstalledSpecs {
			return ErrInstallCatalogInvalid
		}
		if err := validateInstalledRecord(record); err != nil {
			return err
		}
	}
	validation := registry.New()
	tools := make([]registry.ToolRegistration, 0)
	services := make([]registry.ServiceRegistration, 0, len(records))
	for _, record := range records {
		for _, spec := range record.Tools {
			tools = append(tools, registry.ToolRegistration{Spec: spec, Handler: noopInstalledHandler})
		}
		capabilities := make(map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}, len(record.Capabilities))
		for _, spec := range record.Capabilities {
			capabilities[spec.ID] = struct {
				Spec    registry.CapabilitySpec
				Handler registry.Handler
			}{Spec: spec, Handler: noopInstalledHandler}
		}
		services = append(services, registry.ServiceRegistration{Spec: record.Service, Capabilities: capabilities})
	}
	if err := validation.RegisterBatch(tools, services); err != nil {
		return errors.Join(ErrInstallCatalogInvalid, err)
	}
	return nil
}

func validateInstalledRecord(record InstalledRecord) error {
	if err := validateManifest(record.Runtime); err != nil ||
		record.Directory == "" || record.Service.Version != record.Runtime.Version ||
		len(record.Capabilities) == 0 {
		return errors.Join(ErrInstallCatalogInvalid, err)
	}
	for _, tool := range record.Tools {
		if tool.Version != record.Runtime.Version {
			return ErrInstallCatalogInvalid
		}
	}
	for _, capability := range record.Capabilities {
		if capability.Version != record.Runtime.Version || capability.ServiceID != record.Service.ID {
			return ErrInstallCatalogInvalid
		}
	}
	if record.Runtime.Mode == ModeIsolated && record.Process == nil {
		return ErrInstallCatalogInvalid
	}
	if record.Runtime.Mode == ModeHosted && record.Process != nil {
		return ErrInstallCatalogInvalid
	}
	return nil
}

func readSecureJSONFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) ||
		info.Size() <= 0 || info.Size() > maximum {
		return nil, ErrInstallCatalogInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrInstallChanged
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) > maximum {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	return payload, nil
}

func decodeStrictJSON(payload []byte, target any) error {
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := jsonutil.EnsureEOF(decoder); err != nil {
		return errors.Join(ErrInstallCatalogInvalid, err)
	}
	return nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInstallCatalogInvalid
				}
				if _, duplicate := keys[key]; duplicate {
					return ErrInstallCatalogInvalid
				}
				keys[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrInstallCatalogInvalid
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrInstallCatalogInvalid
			}
		default:
			return ErrInstallCatalogInvalid
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInstallCatalogInvalid
	}
	return nil
}

func validateSecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
		return ErrInstallCatalogInvalid
	}
	return nil
}

func validateInstalledPath(root, path string, directory bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", ErrInstallCatalogInvalid
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." && !directory || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrInstallCatalogInvalid
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	for index, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 ||
			!ownerMatchesProcess(info) {
			return "", ErrInstallCatalogInvalid
		}
		last := index == len(parts)-1
		if !last && !info.IsDir() {
			return "", ErrInstallCatalogInvalid
		}
		if last && ((directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular())) {
			return "", ErrInstallCatalogInvalid
		}
	}
	return path, nil
}

func hashInstalledArtifact(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxInstallArtifact {
		return "", ErrInstallCatalogInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", ErrInstallChanged
	}
	digest := sha256.New()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > maxInstallArtifact {
				return "", ErrInstallCatalogInvalid
			}
			if _, err := digest.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func sameRuntimeManifest(left, right Manifest) bool {
	return left.ID == right.ID && left.Version == right.Version && left.Mode == right.Mode &&
		left.Role == right.Role && left.LockedDigest == right.LockedDigest &&
		left.Pin == right.Pin && left.IdleTTL == right.IdleTTL
}

// sameRuntimeIdentity 只比较运行时身份字段（ID/Version/Mode），供 ReadArtifact
// 在装载工件时使用；其余清单字段不参与工件装载，完整清单的一致性由
// VerifyRuntime/readRecord 在各自边界校验。
func sameRuntimeIdentity(left, right Manifest) bool {
	return left.ID == right.ID && left.Version == right.Version && left.Mode == right.Mode
}

func cloneProcessSpec(spec ProcessSpec) ProcessSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	return spec
}

func cloneToolSpecs(specs []registry.ToolSpec) []registry.ToolSpec {
	cloned := append([]registry.ToolSpec(nil), specs...)
	for index := range cloned {
		cloned[index].RequiredPermissions = append([]string(nil), cloned[index].RequiredPermissions...)
	}
	return cloned
}

func cloneCapabilitySpecs(specs []registry.CapabilitySpec) []registry.CapabilitySpec {
	cloned := append([]registry.CapabilitySpec(nil), specs...)
	for index := range cloned {
		cloned[index].RequiredPermissions = append([]string(nil), cloned[index].RequiredPermissions...)
	}
	return cloned
}

func cloneInstalledService(spec registry.ServiceSpec) registry.ServiceSpec {
	spec.ToolDependencies = append([]string(nil), spec.ToolDependencies...)
	spec.RequestedPermissions = append([]string(nil), spec.RequestedPermissions...)
	return spec
}

var noopInstalledHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return nil, ErrUnavailable
}
