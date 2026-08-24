package packmgr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
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

func TestInstallFromReleaseEndToEnd(t *testing.T) {
	// 先打一个真实 tarball，让 mock 服务器直接喂给客户端。
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)
	tarballPath, err := packmgr.Pack(context.Background(), source, t.TempDir())
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

	root := t.TempDir()
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
	writeSourcePackage(t, source, "demo.pkg", "2.0.0", packmgr.ModeHosted, "app.wasm", nil)
	tarballPath, err := packmgr.Pack(context.Background(), source, t.TempDir())
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
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)

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

	htmlURL, err := client.Publish(context.Background(), "owner", "repo", source)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if htmlURL != "https://github.com/owner/repo/releases/tag/v1.0.0" {
		t.Fatalf("html url = %s", htmlURL)
	}
	if !strings.Contains(uploadPath, "demo.pkg-1.0.0.tgz") {
		t.Fatalf("upload path = %s, want asset name demo.pkg-1.0.0.tgz", uploadPath)
	}
}

func TestPublishRemovesReleaseAfterUploadFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)
	var deleted bool
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			deleted = true
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 7, "html_url": "https://example.com/release"})
	}, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	})
	if _, err := client.Publish(context.Background(), "owner", "repo", source); err == nil {
		t.Fatal("Publish upload failure = nil")
	}
	if !deleted {
		t.Fatal("Publish did not remove the incomplete release")
	}
}

func TestPublishRejectsImmutableVersion(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)
	client, _, _ := newGitHubTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
	}, http.NotFound)
	if _, err := client.Publish(context.Background(), "owner", "repo", source); err == nil ||
		!strings.Contains(err.Error(), "不可变") {
		t.Fatalf("Publish duplicate error = %v, want immutable-version error", err)
	}
}

func TestPublishRequiresToken(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)
	client := packmgr.NewGitHubClient()
	client.Token = ""
	if _, err := client.Publish(context.Background(), "owner", "repo", source); err == nil {
		t.Fatal("Publish without token = nil, want error")
	}
}
