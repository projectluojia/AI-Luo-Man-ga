package packmgr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	packageiotest "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio/testutil"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
)

// newGitHubTestClient 构造指向 httptest 后端的客户端（API 与 uploads 分离）。
func newGitHubTestClient(t *testing.T, apiHandler, uploadsHandler http.HandlerFunc) (*packmgr.GitHubClient, *httptest.Server, *httptest.Server) {
	t.Helper()
	api := httptest.NewServer(apiHandler)
	uploads := httptest.NewServer(uploadsHandler)
	t.Cleanup(func() { api.Close(); uploads.Close() })
	client := packmgr.NewGitHubClient()
	client.APIBase = api.URL
	client.UploadBase = uploads.URL
	client.Token = "test-token"
	return client, api, uploads
}

func readSourceManifest(t *testing.T, source string) (packagecontract.Manifest, []byte) {
	t.Helper()
	manifestBytes, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest packagecontract.Manifest
	if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, manifestBytes
}

func TestResolveReleasePicksHighestMatchingConstraint(t *testing.T) {
	ctx := context.Background()
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "/releases") {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]map[string]any{})
			return
		}
		// GitHub API 按新到旧返回 Release。
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"tag_name": "v1.2.0", "assets": []map[string]any{
				{"name": "demo.pkg-1.2.0.tgz", "browser_download_url": "https://example.com/demo.pkg-1.2.0.tgz"},
			}},
			{"tag_name": "v1.0.0", "assets": []map[string]any{
				{"name": "demo.pkg-1.0.0.tgz", "browser_download_url": "https://example.com/demo.pkg-1.0.0.tgz"},
			}},
			{"tag_name": "release-2026", "assets": []map[string]any{}},
		})
	}, http.NotFound)
	version, assetURL, err := client.ResolveRelease(ctx, "owner", "repo", "^1.0.0")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if version != "1.2.0" || assetURL != "https://example.com/demo.pkg-1.2.0.tgz" {
		t.Fatalf("ResolveRelease = %s %s, want 1.2.0 + demo.pkg-1.2.0.tgz", version, assetURL)
	}
	// 最新版（无约束）。
	version, _, err = client.ResolveRelease(ctx, "owner", "repo", "")
	if err != nil || version != "1.2.0" {
		t.Fatalf("ResolveRelease latest = %s err=%v, want 1.2.0", version, err)
	}
	// 不满足约束。
	if _, _, err := client.ResolveRelease(ctx, "owner", "repo", "^2.0.0"); err == nil {
		t.Fatal("ResolveRelease unsatisfied = nil, want error")
	}
}

// TestResolveReleaseIgnoresPublishOrder 覆盖"旧版本线的补丁晚于新主版本发布"：
// GitHub 按创建时间倒序返回，v1.0.1 排在 v1.2.0 之前，取首个匹配项会解析出 1.0.1。
func TestResolveReleaseIgnoresPublishOrder(t *testing.T) {
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"tag_name": "v1.0.1", "assets": []map[string]any{
				{"name": "demo.pkg-1.0.1.tgz", "browser_download_url": "https://example.com/demo.pkg-1.0.1.tgz"},
			}},
			{"tag_name": "v1.2.0", "assets": []map[string]any{
				{"name": "demo.pkg-1.2.0.tgz", "browser_download_url": "https://example.com/demo.pkg-1.2.0.tgz"},
			}},
		})
	}, http.NotFound)
	version, assetURL, err := client.ResolveRelease(context.Background(), "owner", "repo", "^1.0.0")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if version != "1.2.0" || assetURL != "https://example.com/demo.pkg-1.2.0.tgz" {
		t.Fatalf("ResolveRelease = %s %s, want 1.2.0 + demo.pkg-1.2.0.tgz", version, assetURL)
	}
}

// TestResolveReleaseSkipsReleasesWithoutTarball 确认没有 .tgz 资产的更高版本不会
// 顶掉已选中的较低版本：附件缺失的 Release 不可安装，必须保留有 tarball 的版本。
func TestResolveReleaseSkipsReleasesWithoutTarball(t *testing.T) {
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"tag_name": "v1.2.0", "assets": []map[string]any{
				{"name": "demo.pkg-1.2.0.tgz", "browser_download_url": "https://example.com/demo.pkg-1.2.0.tgz"},
			}},
			{"tag_name": "v1.3.0", "assets": []map[string]any{
				{"name": "notes.txt", "browser_download_url": "https://example.com/notes.txt"},
			}},
		})
	}, http.NotFound)
	version, assetURL, err := client.ResolveRelease(context.Background(), "owner", "repo", "")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if version != "1.2.0" || assetURL != "https://example.com/demo.pkg-1.2.0.tgz" {
		t.Fatalf("ResolveRelease = %s %s, want 1.2.0 + demo.pkg-1.2.0.tgz", version, assetURL)
	}
}

func TestResolveReleaseScansAllPages(t *testing.T) {
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		var releases []map[string]any
		switch page {
		case "1", "2":
			releases = []map[string]any{{"tag_name": "v1." + page + ".0", "assets": []map[string]any{{
				"name": "demo.pkg-1." + page + ".0.tgz", "browser_download_url": "https://example.com/1." + page,
			}}}}
		case "3":
			releases = []map[string]any{{"tag_name": "v2.0.0", "assets": []map[string]any{{
				"name": "demo.pkg-2.0.0.tgz", "browser_download_url": "https://example.com/2.0.0",
			}}}}
		}
		_ = json.NewEncoder(writer).Encode(releases)
	}, http.NotFound)
	version, assetURL, err := client.ResolveRelease(context.Background(), "owner", "repo", "")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if version != "2.0.0" || assetURL != "https://example.com/2.0.0" {
		t.Fatalf("ResolveRelease = %s %s, want page-3 release", version, assetURL)
	}
}

func TestResolveReleaseBoundsPagination(t *testing.T) {
	var pages atomic.Int32
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		pages.Add(1)
		_ = json.NewEncoder(writer).Encode([]map[string]any{{"tag_name": "not-semver"}})
	}, http.NotFound)
	if _, _, err := client.ResolveRelease(context.Background(), "owner", "repo", ""); err == nil {
		t.Fatal("ResolveRelease with endless non-semver pages = nil, want error")
	}
	if pages.Load() != 100 {
		t.Fatalf("ResolveRelease pages = %d, want bounded scan of 100 pages", pages.Load())
	}
}

func TestInstallFromReleaseEndToEnd(t *testing.T) {
	// 先打一个真实 tarball，让 mock 服务器直接喂给客户端。
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.pkg", Version: "1.0.0",
		Components: []packagecontract.Component{{ID: "core", Mode: packagecontract.ModeHosted, Role: packagecontract.RoleProvider, Entrypoint: "app.wasm"}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tarballPath, err := packmgr.PackFromSource(context.Background(), source, t.TempDir(), manifest, manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	tarballBytes, err := os.ReadFile(tarballPath)
	if err != nil {
		t.Fatal(err)
	}
	// 资产下载端点。
	assets := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(tarballBytes)
	}))
	t.Cleanup(assets.Close)
	// Release 列表端点。
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			{"tag_name": "v1.0.0", "assets": []map[string]any{
				{"name": "demo.pkg-1.0.0.tgz", "browser_download_url": assets.URL + "/demo.pkg-1.0.0.tgz"},
			}},
		})
	}, http.NotFound)

	root := packageiotest.TempDir(t)
	record, err := packmgr.InstallFromRelease(context.Background(), root, client, "owner", "repo", "^1.0.0")
	if err != nil {
		t.Fatalf("InstallFromRelease: %v", err)
	}
	if record.Manifest.Version != "1.0.0" {
		t.Fatalf("installed version = %s, want 1.0.0", record.Manifest.Version)
	}
}

func TestInstallFromReleaseRejectsManifestVersionBeforeInstall(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "2.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifestBytes, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest packagecontract.Manifest
	if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	tarballPath, err := packmgr.PackFromSource(context.Background(), source, t.TempDir(), manifest, manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	tarballBytes, err := os.ReadFile(tarballPath)
	if err != nil {
		t.Fatal(err)
	}
	assets := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(tarballBytes)
	}))
	t.Cleanup(assets.Close)
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(writer).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{{
			"tag_name": "v1.0.0", "assets": []map[string]any{{
				"name": "demo.pkg-1.0.0.tgz", "browser_download_url": assets.URL,
			}},
		}})
	}, http.NotFound)
	root := t.TempDir()
	if _, err := packmgr.InstallFromRelease(context.Background(), root, client, "owner", "repo", ""); err == nil {
		t.Fatal("InstallFromRelease accepted mismatched manifest version")
	}
	if _, err := os.Stat(filepath.Join(root, "demo.pkg")); !os.IsNotExist(err) {
		t.Fatalf("mismatched release mutated install root: %v", err)
	}
}

func TestPublishCreatesReleaseAndUploadsAsset(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifest, manifestBytes := readSourceManifest(t, source)

	var uploadPath string
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/releases") {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 7, "html_url": "https://github.com/owner/repo/releases/tag/v1.0.0"})
	}, func(writer http.ResponseWriter, request *http.Request) {
		uploadPath = request.URL.RequestURI()
		writer.WriteHeader(http.StatusCreated)
	})

	htmlURL, err := client.PublishFromSource(context.Background(), "owner", "repo", source, manifest, manifestBytes)
	if err != nil {
		t.Fatalf("PublishFromSource: %v", err)
	}
	if htmlURL != "https://github.com/owner/repo/releases/tag/v1.0.0" {
		t.Fatalf("html url = %s", htmlURL)
	}
	if !strings.Contains(uploadPath, "demo.pkg-1.0.0.tgz") {
		t.Fatalf("upload path = %s, want asset name demo.pkg-1.0.0.tgz", uploadPath)
	}
}

func TestPublishTarballCreatesReleaseAndUploadsAsset(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", []packagecontract.Dependency{{ID: "dependency.pkg", Constraint: "^1.0.0", Source: "github:owner/dependency"}})
	manifest, manifestBytes := readSourceManifest(t, source)
	var err error
	tarball, err := packmgr.PackFromSource(context.Background(), source, t.TempDir(), manifest, manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	var uploadPath string
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/releases") {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 9, "html_url": "https://github.com/owner/repo/releases/tag/v1.0.0"})
	}, func(writer http.ResponseWriter, request *http.Request) {
		uploadPath = request.URL.RequestURI()
		writer.WriteHeader(http.StatusCreated)
	})
	if _, err := client.PublishTarball(context.Background(), "owner", "repo", tarball); err != nil {
		t.Fatalf("PublishTarball: %v", err)
	}
	if !strings.Contains(uploadPath, "demo.pkg-1.0.0.tgz") {
		t.Fatalf("upload path = %s, want asset name demo.pkg-1.0.0.tgz", uploadPath)
	}
}

func TestPublishRemovesReleaseAfterUploadFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifest, manifestBytes := readSourceManifest(t, source)
	var deleted atomic.Bool
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 10, "html_url": "https://example.com/release"})
			return
		case http.MethodDelete:
			deleted.Store(true)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := client.PublishFromSource(context.Background(), "owner", "repo", source, manifest, manifestBytes); err == nil {
		t.Fatal("Publish upload failure = nil, want error")
	}
	if !deleted.Load() {
		t.Fatal("Publish did not remove the incomplete release")
	}
}

func TestPublishRejectsImmutableVersion(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifest, manifestBytes := readSourceManifest(t, source)
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
	}, http.NotFound)
	if _, err := client.PublishFromSource(context.Background(), "owner", "repo", source, manifest, manifestBytes); err == nil ||
		!strings.Contains(err.Error(), "不可变") {
		t.Fatalf("Publish duplicate error = %v, want immutable-version error", err)
	}
}

func TestPublishRequiresToken(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packagecontract.ModeHosted, "app.wasm", nil)
	manifest, manifestBytes := readSourceManifest(t, source)
	client := packmgr.NewGitHubClient()
	client.Token = ""
	if _, err := client.PublishFromSource(context.Background(), "owner", "repo", source, manifest, manifestBytes); err == nil {
		t.Fatal("Publish without token = nil, want error")
	}
}
