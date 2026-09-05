package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// packageDocuments 实现 packstore.Store：包装统一 Store 复用其连接与事务
// 互斥。作用域以 (app_id, package-qualified namespace) 复合键强制 App 与包隔离，全部查询参数化；
// 读取在单事务内同时取回文档与快照元数据（快照原子替换下 reader 永不观察
// 到跨修订混合），快照替换在单写事务内原子生效、失败保留上一完整版本。
type packageDocuments struct {
	store *Store
}

// PackageDocuments 返回统一存储的包文档端口视图（ailuo.store 宿主函数背后
// 的持久化端口）。App 隔离由调用方传入的 Scope 强制。
func (s *Store) PackageDocuments() packstore.Store {
	return packageDocuments{store: s}
}

// readSnapshotMetaTx 在给定查询器上读取当前生效快照元数据；无快照时
// metaFound 为 false（文档可由 put 独立写入而尚无快照）。
func readSnapshotMetaTx(ctx context.Context, queryer rowQueryer, scope packstore.Scope) (packstore.SnapshotMeta, bool, error) {
	var meta packstore.SnapshotMeta
	var authoritative, complete int
	var importedAt, validUntil string
	err := queryer.QueryRowContext(ctx, `
SELECT revision, source, authoritative, complete, imported_at, valid_until
FROM package_snapshots
WHERE app_id=? AND namespace=? AND is_current=1`,
		scope.AppID, scope.Namespace).Scan(
		&meta.Revision, &meta.Source, &authoritative, &complete, &importedAt, &validUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return packstore.SnapshotMeta{}, false, nil
	}
	if err != nil {
		return packstore.SnapshotMeta{}, false, fmt.Errorf("read package snapshot meta: %w", err)
	}
	meta.Authoritative = authoritative == 1
	meta.Complete = complete == 1
	if meta.ImportedAt, err = time.Parse(time.RFC3339Nano, importedAt); err != nil {
		return packstore.SnapshotMeta{}, false, fmt.Errorf("parse package snapshot import time: %w", err)
	}
	if meta.ValidUntil, err = time.Parse(time.RFC3339Nano, validUntil); err != nil {
		return packstore.SnapshotMeta{}, false, fmt.Errorf("parse package snapshot validity: %w", err)
	}
	return meta, true, nil
}

func (p packageDocuments) Get(ctx context.Context, scope packstore.Scope, collection, id string) (read packstore.DocumentRead, resultErr error) {
	if err := packstore.ValidateScope(scope); err != nil {
		return packstore.DocumentRead{}, err
	}
	if err := packstore.ValidateCollection(collection); err != nil {
		return packstore.DocumentRead{}, err
	}
	if err := packstore.ValidateDocID(id); err != nil {
		return packstore.DocumentRead{}, err
	}
	tx, err := p.store.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return packstore.DocumentRead{}, fmt.Errorf("begin package document get: %w", err)
	}
	defer p.store.finishTx(tx, &resultErr, "get package document")
	meta, metaFound, err := readSnapshotMetaTx(ctx, tx, scope)
	if err != nil {
		return packstore.DocumentRead{}, err
	}
	var payload string
	err = tx.QueryRowContext(ctx, `
SELECT payload FROM package_documents
WHERE app_id=? AND namespace=? AND collection=? AND doc_id=?`,
		scope.AppID, scope.Namespace, collection, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return packstore.DocumentRead{Meta: meta, MetaFound: metaFound, Found: false}, nil
	}
	if err != nil {
		return packstore.DocumentRead{}, fmt.Errorf("read package document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return packstore.DocumentRead{}, fmt.Errorf("commit package document get: %w", err)
	}
	return packstore.DocumentRead{
		Meta: meta, MetaFound: metaFound, Found: true,
		Document: packstore.Document{ID: id, Payload: json.RawMessage(payload)},
	}, nil
}

func (p packageDocuments) Put(ctx context.Context, scope packstore.Scope, collection, id string, payload json.RawMessage) (resultErr error) {
	if err := packstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := packstore.ValidateCollection(collection); err != nil {
		return err
	}
	if err := packstore.ValidateDocID(id); err != nil {
		return err
	}
	if err := packstore.ValidatePayload(payload); err != nil {
		return err
	}
	tx, err := p.store.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin package document put: %w", err)
	}
	defer p.store.finishTx(tx, &resultErr, "put package document")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO package_documents(app_id,namespace,collection,doc_id,payload,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(app_id,namespace,collection,doc_id) DO UPDATE SET
  payload=excluded.payload, updated_at=excluded.updated_at`,
		scope.AppID, scope.Namespace, collection, id, string(payload),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("write package document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit package document put: %w", err)
	}
	return nil
}

func (p packageDocuments) Delete(ctx context.Context, scope packstore.Scope, collection, id string) (resultErr error) {
	if err := packstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := packstore.ValidateCollection(collection); err != nil {
		return err
	}
	if err := packstore.ValidateDocID(id); err != nil {
		return err
	}
	tx, err := p.store.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin package document delete: %w", err)
	}
	defer p.store.finishTx(tx, &resultErr, "delete package document")
	result, err := tx.ExecContext(ctx, `
DELETE FROM package_documents
WHERE app_id=? AND namespace=? AND collection=? AND doc_id=?`,
		scope.AppID, scope.Namespace, collection, id)
	if err != nil {
		return fmt.Errorf("delete package document: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count package document delete: %w", err)
	}
	if affected == 0 {
		return packstore.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit package document delete: %w", err)
	}
	return nil
}

func (p packageDocuments) List(ctx context.Context, scope packstore.Scope, collection string, limit int, afterID string) (read packstore.CollectionRead, resultErr error) {
	if err := packstore.ValidateScope(scope); err != nil {
		return packstore.CollectionRead{}, err
	}
	if err := packstore.ValidateCollection(collection); err != nil {
		return packstore.CollectionRead{}, err
	}
	if limit < 1 || limit > packstore.MaxListLimit {
		return packstore.CollectionRead{}, packstore.ErrInvalidKey
	}
	if afterID != "" {
		if err := packstore.ValidateDocID(afterID); err != nil {
			return packstore.CollectionRead{}, err
		}
	}
	tx, err := p.store.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return packstore.CollectionRead{}, fmt.Errorf("begin package document list: %w", err)
	}
	defer p.store.finishTx(tx, &resultErr, "list package documents")
	meta, metaFound, err := readSnapshotMetaTx(ctx, tx, scope)
	if err != nil {
		return packstore.CollectionRead{}, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT doc_id, payload FROM package_documents
WHERE app_id=? AND namespace=? AND collection=? AND doc_id > ?
ORDER BY doc_id LIMIT ?`,
		scope.AppID, scope.Namespace, collection, afterID, limit)
	if err != nil {
		return packstore.CollectionRead{}, fmt.Errorf("list package documents: %w", err)
	}
	documents := make([]packstore.Document, 0, limit)
	for rows.Next() {
		var document packstore.Document
		var payload string
		if err := rows.Scan(&document.ID, &payload); err != nil {
			rows.Close()
			return packstore.CollectionRead{}, fmt.Errorf("scan package document: %w", err)
		}
		document.Payload = json.RawMessage(payload)
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return packstore.CollectionRead{}, err
	}
	if err := rows.Close(); err != nil {
		return packstore.CollectionRead{}, fmt.Errorf("close package document rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return packstore.CollectionRead{}, fmt.Errorf("commit package document list: %w", err)
	}
	return packstore.CollectionRead{Meta: meta, MetaFound: metaFound, Documents: documents}, nil
}

func (p packageDocuments) ReplaceSnapshot(ctx context.Context, scope packstore.Scope, meta packstore.SnapshotMeta, collections map[string][]packstore.Document) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "replace_package_snapshot", started, resultErr) }()
	if err := packstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := packstore.ValidateSnapshot(meta, collections); err != nil {
		return err
	}
	tx, err := p.store.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin package snapshot replace: %w", err)
	}
	defer p.store.finishTx(tx, &resultErr, "replace package snapshot")
	// 先停用旧快照再写入：任一步失败整体回滚，保留上一完整版本。
	if _, err := tx.ExecContext(ctx, `
UPDATE package_snapshots SET is_current=0
WHERE app_id=? AND namespace=? AND is_current=1`, scope.AppID, scope.Namespace); err != nil {
		return fmt.Errorf("deactivate package snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM package_documents WHERE app_id=? AND namespace=?`,
		scope.AppID, scope.Namespace); err != nil {
		return fmt.Errorf("clear package documents: %w", err)
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for collection, documents := range collections {
		for _, document := range documents {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO package_documents(app_id,namespace,collection,doc_id,payload,updated_at)
VALUES(?,?,?,?,?,?)`,
				scope.AppID, scope.Namespace, collection, document.ID, string(document.Payload), updatedAt); err != nil {
				return fmt.Errorf("insert package document %q/%q: %w", collection, document.ID, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO package_snapshots(app_id,namespace,revision,source,authoritative,complete,imported_at,valid_until,is_current)
VALUES(?,?,?,?,?,?,?,?,1)
ON CONFLICT(app_id,namespace,revision) DO UPDATE SET
  source=excluded.source, authoritative=excluded.authoritative, complete=excluded.complete,
  imported_at=excluded.imported_at, valid_until=excluded.valid_until, is_current=1`,
		scope.AppID, scope.Namespace, meta.Revision, meta.Source,
		boolInt(meta.Authoritative), boolInt(meta.Complete),
		meta.ImportedAt.UTC().Format(time.RFC3339Nano), meta.ValidUntil.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("activate package snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit package snapshot replace: %w", err)
	}
	observe.Info(ctx, "包数据快照已经原子替换",
		observe.Component("storage"),
		observe.StringAttr("app_id", scope.AppID),
		observe.StringAttr("namespace", scope.Namespace),
		observe.StringAttr("source_revision", meta.Revision),
		observe.StringAttr("source", meta.Source),
		observe.BoolAttr("authoritative", meta.Authoritative),
		observe.IntAttr("collection_count", len(collections)),
		observe.Duration(started),
	)
	return nil
}
