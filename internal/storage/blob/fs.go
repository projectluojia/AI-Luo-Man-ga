// Package blob 提供消息正文与附件内容的本地文件系统 BlobStore 适配器。
//
// 安全模型：所有 Blob 操作经 os.Root 句柄执行，符号链接无法逃逸根目录；
// blobID 在进入文件系统前必须通过 session.ValidBlobID 校验（拒绝空段、
// "."、".."、绝对路径与反斜杠）。写入前校验大小上限，读取前校验磁盘大小，
// 避免超大文件导致内存膨胀。
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// Store 是 BlobStore 端口的本地文件系统实现。
type Store struct {
	root     *os.Root
	rootPath string
	maxBytes int64
}

// Open 打开（必要时创建）根目录并返回 Blob 存储。maxBytes 为单个 Blob 的大小上限。
func Open(rootDir string, maxBytes int64) (*Store, error) {
	if rootDir == "" || maxBytes <= 0 || maxBytes > session.MaxMessageContentBytes {
		return nil, fmt.Errorf("invalid blob storage configuration")
	}
	absolute, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve blob root path: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create blob root directory: %w", err)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open blob root: %w", err)
	}
	return &Store{root: root, rootPath: absolute, maxBytes: maxBytes}, nil
}

// Close 关闭根目录句柄。
func (b *Store) Close() error {
	return b.root.Close()
}

// Put 写入 Blob 内容。经 os.Root 句柄直接写入目标文件，符号链接逃逸由 os.Root
// 拒绝；调用方保证元数据行只在 Put 成功之后发布，因此系统内的读者不会观察到
// 部分写入的内容。
func (b *Store) Put(ctx context.Context, blobID string, content []byte) (resultErr error) {
	started := time.Now()
	defer func() { observeBlobOperation(ctx, "blob_put", started, resultErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validateBlobID(blobID); err != nil {
		return err
	}
	if int64(len(content)) > b.maxBytes {
		return session.ErrBlobTooLarge
	}
	rel := filepath.FromSlash(blobID)
	parent := filepath.Dir(rel)
	if parent != "." {
		if err := b.root.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create blob parent directory: %w", err)
		}
	}
	file, err := b.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open blob destination: %w", err)
	}
	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write blob content: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close blob content: %w", closeErr)
	}
	return nil
}

// Get 读取 Blob 内容。文件不存在或已删除返回 ErrBlobNotFound；
// 磁盘大小超过存储上限返回 ErrBlobTooLarge。
func (b *Store) Get(ctx context.Context, blobID string) (_ []byte, resultErr error) {
	started := time.Now()
	defer func() { observeBlobOperation(ctx, "blob_get", started, resultErr) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validateBlobID(blobID); err != nil {
		return nil, err
	}
	file, err := b.root.Open(filepath.FromSlash(blobID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, session.ErrBlobNotFound
		}
		return nil, fmt.Errorf("open blob content: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat blob content: %w", err)
	}
	if info.Size() > b.maxBytes {
		file.Close()
		return nil, session.ErrBlobTooLarge
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read blob content: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close blob content: %w", closeErr)
	}
	return content, nil
}

// Delete 删除 Blob；删除后不可再读取。不存在的 Blob 返回 ErrBlobNotFound。
func (b *Store) Delete(ctx context.Context, blobID string) (resultErr error) {
	started := time.Now()
	defer func() { observeBlobOperation(ctx, "blob_delete", started, resultErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validateBlobID(blobID); err != nil {
		return err
	}
	err := b.root.Remove(filepath.FromSlash(blobID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return session.ErrBlobNotFound
		}
		return fmt.Errorf("delete blob content: %w", err)
	}
	return nil
}

// validateBlobID 校验 Blob 标识。文件系统适配器进一步拒绝 ':'，
// 因为 Windows 文件名字符集不允许冒号，避免同一 blobID 在不同平台产生不同行为。
func (b *Store) validateBlobID(blobID string) error {
	if !session.ValidBlobID(blobID) {
		return session.ErrInvalidBlobRef
	}
	if strings.Contains(blobID, ":") {
		return session.ErrInvalidBlobRef
	}
	return nil
}

func observeBlobOperation(ctx context.Context, operation string, started time.Time, err error) {
	if err != nil {
		observe.Error(ctx, "Blob 存储操作失败", err,
			observe.StringAttr("blob_operation", operation),
			observe.Duration(started),
		)
		return
	}
	observe.Debug(ctx, "Blob 存储操作完成",
		observe.StringAttr("blob_operation", operation),
		observe.Duration(started),
	)
}
