package blob_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
)

func openBlobStore(t *testing.T, maxBytes int64) (*blob.Store, string) {
	t.Helper()
	rootDir := t.TempDir()
	store, err := blob.Open(rootDir, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, rootDir
}

func TestBlobPutGetDeleteRoundTrip(t *testing.T) {
	store, _ := openBlobStore(t, 1024)
	ctx := context.Background()
	body := []byte("图片附件二进制正文")

	if err := store.Put(ctx, "attachments/att-1", body); err != nil {
		t.Fatalf("写入 Blob：%v", err)
	}
	got, err := store.Get(ctx, "attachments/att-1")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("Blob 读取=%q err=%v", got, err)
	}
	// 覆盖写入幂等。
	if err := store.Put(ctx, "attachments/att-1", body); err != nil {
		t.Fatalf("重复写入 Blob：%v", err)
	}
	if err := store.Delete(ctx, "attachments/att-1"); err != nil {
		t.Fatalf("删除 Blob：%v", err)
	}
	// 删除后不可读。
	if _, err := store.Get(ctx, "attachments/att-1"); !errors.Is(err, session.ErrBlobNotFound) {
		t.Fatalf("删除后读取 error=%v, want ErrBlobNotFound", err)
	}
	if err := store.Delete(ctx, "attachments/att-1"); !errors.Is(err, session.ErrBlobNotFound) {
		t.Fatalf("重复删除 error=%v, want ErrBlobNotFound", err)
	}
}

func TestBlobRejectsPathTraversalAndAbsolutePaths(t *testing.T) {
	store, rootDir := openBlobStore(t, 1024)
	ctx := context.Background()
	outside := t.TempDir()
	secret := "outside-secret"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	// 允许的命名空间路径正常工作。
	if err := store.Put(ctx, "messages/msg-1", []byte("ok")); err != nil {
		t.Fatalf("合法 Blob 路径被拒绝：%v", err)
	}
	for _, blobID := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"a/../b",
		"..",
		".",
		"a/..",
		"/abs",
		"abs/",
		"",
		"a\\..\\b",
		"messages:secret",
		"C:/windows/system.ini",
		strings.Repeat("x", 257),
	} {
		if err := store.Put(ctx, blobID, []byte("bad")); !errors.Is(err, session.ErrInvalidBlobRef) {
			t.Errorf("Put(%q) error=%v, want ErrInvalidBlobRef", blobID, err)
		}
		if _, err := store.Get(ctx, blobID); !errors.Is(err, session.ErrInvalidBlobRef) {
			t.Errorf("Get(%q) error=%v, want ErrInvalidBlobRef", blobID, err)
		}
		if err := store.Delete(ctx, blobID); !errors.Is(err, session.ErrInvalidBlobRef) {
			t.Errorf("Delete(%q) error=%v, want ErrInvalidBlobRef", blobID, err)
		}
	}
	// 穿越路径不得在根目录外产生任何文件。
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("外部秘密文件丢失：%v", err)
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "messages" {
			t.Fatalf("根目录出现意外内容 %q", entry.Name())
		}
	}
}

func TestBlobRejectsOversizedContent(t *testing.T) {
	store, rootDir := openBlobStore(t, 64)
	ctx := context.Background()
	body := bytes.Repeat([]byte("x"), 65)
	if err := store.Put(ctx, "blob", body); !errors.Is(err, session.ErrBlobTooLarge) {
		t.Fatalf("超限写入 error=%v, want ErrBlobTooLarge", err)
	}
	// 直接落盘也无法绕过读取上限（防篡改）。
	if err := os.WriteFile(filepath.Join(rootDir, "tampered"), bytes.Repeat([]byte("y"), 65), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "tampered"); !errors.Is(err, session.ErrBlobTooLarge) {
		t.Fatalf("超限读取 error=%v, want ErrBlobTooLarge", err)
	}
	// 恰好等于上限允许。
	if err := store.Put(ctx, "blob", bytes.Repeat([]byte("x"), 64)); err != nil {
		t.Fatalf("上限内写入被拒绝：%v", err)
	}
}

func TestBlobRejectsSymlinkEscape(t *testing.T) {
	store, rootDir := openBlobStore(t, 1024)
	ctx := context.Background()
	outside := t.TempDir()
	secret := []byte("根目录外的秘密内容")
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	// 在根目录内放置指向外部的符号链接；无法创建链接的环境跳过本用例。
	linkPath := filepath.Join(rootDir, "escape")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("当前环境无法创建符号链接：%v", err)
	}
	// 通过符号链接读取根目录外内容必须失败，且不得泄露秘密内容。
	content, err := store.Get(ctx, "escape/secret.txt")
	if err == nil {
		t.Fatalf("符号链接逃逸未被拒绝，读到 %q", content)
	}
	if bytes.Contains(content, secret) {
		t.Fatal("通过符号链接泄露了根目录外内容")
	}
	// 通过符号链接写入根目录外必须失败。
	if err := store.Put(ctx, "escape/evil.txt", []byte("evil")); err == nil {
		t.Fatal("符号链接逃逸写入未被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("内容被写入了根目录之外")
	}
	// 直接读取外部文件仍然可用，证明失败确实来自沙箱拦截而非路径失效。
	if direct, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || !bytes.Equal(direct, secret) {
		t.Fatalf("外部文件状态异常 direct=%q err=%v", direct, err)
	}
}

func TestBlobRejectsInvalidConfiguredRoot(t *testing.T) {
	if _, err := blob.Open(t.TempDir(), 0); err == nil {
		t.Fatal("非正数大小上限应被拒绝")
	}
	if _, err := blob.Open(t.TempDir(), session.MaxMessageContentBytes+1); err == nil {
		t.Fatal("超过消息正文上限的大小上限应被拒绝")
	}
	if _, err := blob.Open("", 100); err == nil {
		t.Fatal("空根目录应被拒绝")
	}
}
