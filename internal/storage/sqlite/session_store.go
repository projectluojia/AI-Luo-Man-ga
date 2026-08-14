package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
)

func init() {
	// 会话模块的前向迁移固定为版本 16；版本 15 由身份模块占用，
	// 版本 17 由确认模块占用，合并后迁移序列 1–18 连续。
	registerMigration(16, `
CREATE TABLE sessions (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  type TEXT NOT NULL CHECK(type IN ('direct','group','system')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL CHECK(julianday(updated_at) >= julianday(created_at)),
  PRIMARY KEY (app_id, session_id)
);
CREATE TABLE session_members (
  app_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  role TEXT NOT NULL CHECK(role IN ('owner','admin','member')),
  joined_at TEXT NOT NULL,
  PRIMARY KEY (app_id, session_id, user_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE
);
CREATE TABLE session_bindings (
  app_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  platform TEXT NOT NULL CHECK(length(platform) BETWEEN 1 AND 64),
  platform_id TEXT NOT NULL CHECK(length(platform_id) BETWEEN 1 AND 256),
  bound_at TEXT NOT NULL,
  PRIMARY KEY (app_id, session_id, platform, platform_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE
);
CREATE TABLE messages (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  sender_user_id TEXT NOT NULL CHECK(length(sender_user_id) BETWEEN 1 AND 128),
  type TEXT NOT NULL CHECK(type IN ('text','image','file','system','event')),
  content_mode TEXT NOT NULL CHECK(content_mode IN ('inline','blob')),
  content_blob_id TEXT NOT NULL DEFAULT '',
  content_size INTEGER NOT NULL CHECK(content_size > 0),
  content BLOB NOT NULL,
  reply_to TEXT NOT NULL DEFAULT '',
  platform_message_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  edited_at TEXT,
  deleted_at TEXT,
  PRIMARY KEY (app_id, message_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE,
  CHECK(
    (content_mode='inline' AND content_blob_id='' AND content_size <= 262144 AND length(content) BETWEEN 1 AND 262144 AND content_size = length(content)) OR
    (content_mode='blob' AND content_blob_id <> '' AND content_size <= 16777216 AND length(content) = 0)
  )
);
CREATE UNIQUE INDEX messages_platform_dedup_idx
  ON messages(app_id, platform_message_id)
  WHERE platform_message_id <> '';
CREATE INDEX messages_history_idx ON messages(app_id, session_id, created_at, message_id);
CREATE INDEX messages_sender_idx ON messages(app_id, session_id, sender_user_id, created_at);
CREATE TABLE attachments (
  app_id TEXT NOT NULL,
  attachment_id TEXT NOT NULL CHECK(length(attachment_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL CHECK(length(session_id) BETWEEN 1 AND 128),
  message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
  uploader_user_id TEXT NOT NULL CHECK(length(uploader_user_id) BETWEEN 1 AND 128),
  filename TEXT NOT NULL CHECK(length(filename) BETWEEN 1 AND 256),
  mime_type TEXT NOT NULL CHECK(length(mime_type) BETWEEN 1 AND 128),
  size INTEGER NOT NULL CHECK(size > 0 AND size <= 16777216),
  blob_id TEXT NOT NULL CHECK(length(blob_id) BETWEEN 1 AND 256),
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, attachment_id),
  FOREIGN KEY (app_id, session_id) REFERENCES sessions(app_id, session_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, message_id) REFERENCES messages(app_id, message_id) ON DELETE CASCADE
);
`)
}

var _ session.Store = (*Store)(nil)

const messageMetadataSelect = `SELECT app_id,message_id,session_id,sender_user_id,type,content_mode,content_blob_id,content_size,reply_to,platform_message_id,created_at,edited_at,deleted_at FROM messages `

const messageSelect = `SELECT app_id,message_id,session_id,sender_user_id,type,content_mode,content_blob_id,content_size,content,reply_to,platform_message_id,created_at,edited_at,deleted_at FROM messages `

func (s *Store) CreateSession(ctx context.Context, sess session.Session) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_session", started, resultErr) }()
	if err := session.ValidateSession(sess); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session creation: %w", err)
	}
	defer finishTransaction(tx, &resultErr, "create session")
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(app_id,session_id,type,created_at,updated_at) VALUES(?,?,?,?,?)`,
		sess.AppID, sess.SessionID, sess.Type,
		sess.CreatedAt.UTC().Format(time.RFC3339Nano), sess.UpdatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraint(err) {
			existing, readErr := readSession(ctx, tx, sess.AppID, sess.SessionID)
			if readErr != nil {
				return fmt.Errorf("read conflicting session: %w", readErr)
			}
			if sessionsEqual(existing, sess) {
				return nil
			}
			return session.ErrSessionExists
		}
		return fmt.Errorf("insert session: %w", err)
	}
	for _, member := range sess.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_members(app_id,session_id,user_id,role,joined_at) VALUES(?,?,?,?,?)`,
			sess.AppID, sess.SessionID, member.UserID, member.Role, member.JoinedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert session member: %w", err)
		}
	}
	for _, binding := range sess.PlatformBindings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_bindings(app_id,session_id,platform,platform_id,bound_at) VALUES(?,?,?,?,?)`,
			sess.AppID, sess.SessionID, binding.Platform, binding.PlatformID, binding.BoundAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert session binding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session creation: %w", err)
	}
	return nil
}

// EnsureSession 原子创建会话，或在会话类型与平台绑定完全一致时补写成员。
// 该操作供多入口 Hub 使用，避免“先创建再忽略已存在”导致群成员和绑定丢失。
func (s *Store) EnsureSession(ctx context.Context, sess session.Session) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "ensure_session", started, resultErr) }()
	if err := session.ValidateSession(sess); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session ensure: %w", err)
	}
	defer finishTransaction(tx, &resultErr, "ensure session")
	result, err := tx.ExecContext(ctx, `INSERT INTO sessions(app_id,session_id,type,created_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(app_id,session_id) DO NOTHING`,
		sess.AppID, sess.SessionID, sess.Type,
		sess.CreatedAt.UTC().Format(time.RFC3339Nano), sess.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert ensured session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read ensured session result: %w", err)
	}
	if rows == 1 {
		if err := insertSessionRelations(ctx, tx, sess); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit ensured session creation: %w", err)
		}
		return nil
	}
	existing, err := readSession(ctx, tx, sess.AppID, sess.SessionID)
	if err != nil {
		return fmt.Errorf("read ensured session: %w", err)
	}
	if existing.Type != sess.Type || !sessionBindingsEqual(existing.PlatformBindings, sess.PlatformBindings) {
		return session.ErrSessionExists
	}
	existingMembers := make(map[string]string, len(existing.Members))
	for _, member := range existing.Members {
		existingMembers[member.UserID] = member.Role
	}
	added := false
	for _, member := range sess.Members {
		if role, ok := existingMembers[member.UserID]; ok {
			if role != member.Role {
				return session.ErrSessionExists
			}
			continue
		}
		if len(existingMembers) >= session.MaxMembersPerSession {
			return session.ErrInvalidSession
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_members(app_id,session_id,user_id,role,joined_at) VALUES(?,?,?,?,?)`,
			sess.AppID, sess.SessionID, member.UserID, member.Role, member.JoinedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			if isUniqueConstraint(err) {
				return session.ErrSessionExists
			}
			return fmt.Errorf("insert ensured session member: %w", err)
		}
		existingMembers[member.UserID] = member.Role
		added = true
	}
	if added {
		updatedAt := sess.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=CASE WHEN julianday(updated_at) < julianday(?) THEN ? ELSE updated_at END WHERE app_id=? AND session_id=?`,
			updatedAt, updatedAt, sess.AppID, sess.SessionID,
		); err != nil {
			return fmt.Errorf("update ensured session: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ensured session: %w", err)
	}
	return nil
}

func insertSessionRelations(ctx context.Context, tx *sql.Tx, sess session.Session) error {
	for _, member := range sess.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_members(app_id,session_id,user_id,role,joined_at) VALUES(?,?,?,?,?)`,
			sess.AppID, sess.SessionID, member.UserID, member.Role, member.JoinedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert ensured session member: %w", err)
		}
	}
	for _, binding := range sess.PlatformBindings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_bindings(app_id,session_id,platform,platform_id,bound_at) VALUES(?,?,?,?,?)`,
			sess.AppID, sess.SessionID, binding.Platform, binding.PlatformID, binding.BoundAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert ensured session binding: %w", err)
		}
	}
	return nil
}

func sessionBindingsEqual(left, right []session.PlatformBinding) bool {
	if len(left) != len(right) {
		return false
	}
	bindings := make(map[string]struct{}, len(left))
	for _, binding := range left {
		bindings[binding.Platform+"\x1f"+binding.PlatformID] = struct{}{}
	}
	for _, binding := range right {
		if _, ok := bindings[binding.Platform+"\x1f"+binding.PlatformID]; !ok {
			return false
		}
	}
	return true
}

func (s *Store) GetSession(ctx context.Context, appID, sessionID string) (result session.Session, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_session", started, resultErr) }()
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) {
		return session.Session{}, session.ErrSessionNotFound
	}
	sess, err := readSession(ctx, s.db, appID, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, session.ErrSessionNotFound
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

func (s *Store) CreateMessage(ctx context.Context, message session.Message, content []byte) (_ session.Message, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_message", started, resultErr) }()
	if err := session.ValidateMessage(message); err != nil {
		return session.Message{}, false, err
	}
	switch message.ContentRef.Mode {
	case session.ContentModeInline:
		if len(content) == 0 || int64(len(content)) != message.ContentRef.Size {
			return session.Message{}, false, session.ErrInvalidMessage
		}
	case session.ContentModeBlob:
		if len(content) != 0 {
			return session.Message{}, false, session.ErrInvalidMessage
		}
	default:
		return session.Message{}, false, session.ErrInvalidMessage
	}
	// Blob 模式不携带正文，但 content 列 NOT NULL：用空字节串而非 nil 写入。
	if content == nil {
		content = []byte{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Message{}, false, fmt.Errorf("begin message creation: %w", err)
	}
	defer finishTransaction(tx, &resultErr, "create message")
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(app_id,message_id,session_id,sender_user_id,type,content_mode,content_blob_id,content_size,content,reply_to,platform_message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		message.AppID, message.MessageID, message.SessionID, message.SenderUserID, message.Type,
		message.ContentRef.Mode, message.ContentRef.BlobID, message.ContentRef.Size, content,
		message.ReplyTo, message.PlatformMessageID, message.CreatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraint(err) {
			existing, existingContent, readErr := readMessage(ctx, tx, message.AppID, message.MessageID)
			if readErr == nil {
				if messageIdentityEqual(existing, existingContent, message, content) {
					return existing, false, nil
				}
				return session.Message{}, false, session.ErrMessageConflict
			}
			if !errors.Is(readErr, sql.ErrNoRows) {
				return session.Message{}, false, fmt.Errorf("read conflicting message: %w", readErr)
			}
			// 唯一约束来自平台去重索引：仅当平台标识非空时才存在该索引冲突。
			if message.PlatformMessageID != "" {
				if _, _, readErr := readMessageByPlatform(ctx, tx, message.AppID, message.PlatformMessageID); readErr == nil {
					return session.Message{}, false, session.ErrMessageConflict
				}
				return session.Message{}, false, fmt.Errorf("read conflicting platform message: %w", readErr)
			}
			return session.Message{}, false, fmt.Errorf("insert message conflict: %w", err)
		}
		if isForeignKeyConstraint(err) {
			return session.Message{}, false, session.ErrSessionNotFound
		}
		return session.Message{}, false, fmt.Errorf("insert message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return session.Message{}, false, fmt.Errorf("commit message creation: %w", err)
	}
	return message, true, nil
}

func (s *Store) GetMessage(ctx context.Context, appID, sessionID, messageID string) (_ session.Message, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_message", started, resultErr) }()
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) || !session.ValidStableID(messageID) {
		return session.Message{}, session.ErrMessageNotFound
	}
	message, _, err := readMessage(ctx, s.db, appID, messageID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session.Message{}, session.ErrMessageNotFound
		}
		return session.Message{}, fmt.Errorf("get message: %w", err)
	}
	if message.SessionID != sessionID || message.DeletedAt != nil {
		return session.Message{}, session.ErrMessageNotFound
	}
	return message, nil
}

func (s *Store) ListMessages(ctx context.Context, appID, sessionID string, query session.MessageQuery) (_ []session.Message, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_messages", started, resultErr) }()
	if err := session.ValidateMessageQuery(query); err != nil {
		return nil, err
	}
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) {
		return nil, session.ErrInvalidMessage
	}
	// 历史查询同时约束 app_id 与 session_id，并排除已删除消息。
	// 默认按时间升序（最旧在前）；Descending 为 true 时按时间倒序（最新在前），
	// 供上下文装配按预算读取最近历史。
	sqlText := messageMetadataSelect + `WHERE app_id=? AND session_id=? AND deleted_at IS NULL`
	args := []any{appID, sessionID}
	if query.SenderUserID != "" {
		sqlText += ` AND sender_user_id=?`
		args = append(args, query.SenderUserID)
	}
	if query.Descending {
		sqlText += ` ORDER BY created_at DESC,message_id DESC LIMIT ?`
	} else {
		sqlText += ` ORDER BY created_at,message_id LIMIT ?`
	}
	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query message history: %w", err)
	}
	defer rows.Close()
	messages := make([]session.Message, 0)
	for rows.Next() {
		message, err := scanMessageMetadata(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message history: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message history: %w", err)
	}
	return messages, nil
}

func (s *Store) GetMessageContent(ctx context.Context, appID, sessionID, messageID string) (_ []byte, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_message_content", started, resultErr) }()
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) || !session.ValidStableID(messageID) {
		return nil, session.ErrMessageNotFound
	}
	var mode string
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT content_mode,content FROM messages WHERE app_id=? AND session_id=? AND message_id=? AND deleted_at IS NULL`,
		appID, sessionID, messageID,
	).Scan(&mode, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, session.ErrMessageNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read message content: %w", err)
	}
	if mode == session.ContentModeBlob {
		return nil, session.ErrMessageContentBlob
	}
	return content, nil
}

func (s *Store) EditMessage(ctx context.Context, edit session.MessageEdit, content []byte) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "edit_message", started, resultErr) }()
	if err := session.ValidateMessageEdit(edit); err != nil {
		return err
	}
	switch edit.NewContentRef.Mode {
	case session.ContentModeInline:
		if len(content) == 0 || int64(len(content)) != edit.NewContentRef.Size {
			return session.ErrInvalidMessage
		}
	case session.ContentModeBlob:
		if len(content) != 0 {
			return session.ErrInvalidMessage
		}
	default:
		return session.ErrInvalidMessage
	}
	// Blob 模式不携带正文，但 content 列 NOT NULL：用空字节串而非 nil 写入。
	if content == nil {
		content = []byte{}
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE messages SET content_mode=?,content_blob_id=?,content_size=?,content=?,edited_at=?
WHERE app_id=? AND session_id=? AND message_id=? AND deleted_at IS NULL`,
		edit.NewContentRef.Mode, edit.NewContentRef.BlobID, edit.NewContentRef.Size, content,
		edit.EditedAt.UTC().Format(time.RFC3339Nano),
		edit.AppID, edit.SessionID, edit.MessageID,
	)
	if err != nil {
		return fmt.Errorf("edit message: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read edited message count: %w", err)
	}
	if affected == 0 {
		var deletedAt sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT deleted_at FROM messages WHERE app_id=? AND session_id=? AND message_id=?`,
			edit.AppID, edit.SessionID, edit.MessageID,
		).Scan(&deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return session.ErrMessageNotFound
		}
		if err != nil {
			return fmt.Errorf("check edited message state: %w", err)
		}
		return session.ErrInvalidTransition
	}
	return nil
}

func (s *Store) RecallMessage(ctx context.Context, appID, sessionID, messageID string, recalledAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "recall_message", started, resultErr) }()
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) || !session.ValidStableID(messageID) || recalledAt.IsZero() {
		return session.ErrInvalidMessage
	}
	result, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE app_id=? AND session_id=? AND message_id=? AND deleted_at IS NULL`,
		recalledAt.UTC().Format(time.RFC3339Nano), appID, sessionID, messageID,
	)
	if err != nil {
		return fmt.Errorf("recall message: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read recalled message count: %w", err)
	}
	if affected == 0 {
		var deletedAt sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT deleted_at FROM messages WHERE app_id=? AND session_id=? AND message_id=?`,
			appID, sessionID, messageID,
		).Scan(&deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return session.ErrMessageNotFound
		}
		if err != nil {
			return fmt.Errorf("check recalled message state: %w", err)
		}
		return session.ErrInvalidTransition
	}
	return nil
}

func (s *Store) CreateAttachment(ctx context.Context, attachment session.Attachment) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_attachment", started, resultErr) }()
	if err := session.ValidateAttachment(attachment); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin attachment creation: %w", err)
	}
	defer finishTransaction(tx, &resultErr, "create attachment")
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(app_id,attachment_id,session_id,message_id,uploader_user_id,filename,mime_type,size,blob_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		attachment.AppID, attachment.AttachmentID, attachment.SessionID, attachment.MessageID, attachment.UploaderUserID,
		attachment.Ref.Filename, attachment.Ref.MimeType, attachment.Ref.Size, attachment.Ref.BlobID,
		attachment.CreatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		if isForeignKeyConstraint(err) {
			var sessionCount int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE app_id=? AND session_id=?`,
				attachment.AppID, attachment.SessionID,
			).Scan(&sessionCount); err != nil {
				return fmt.Errorf("check attachment session reference: %w", err)
			}
			if sessionCount == 0 {
				return session.ErrSessionNotFound
			}
			return session.ErrMessageNotFound
		}
		return fmt.Errorf("insert attachment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit attachment creation: %w", err)
	}
	return nil
}

func (s *Store) GetAttachment(ctx context.Context, appID, sessionID, attachmentID string) (_ session.Attachment, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_attachment", started, resultErr) }()
	if !session.ValidStableID(appID) || !session.ValidStableID(sessionID) || !session.ValidStableID(attachmentID) {
		return session.Attachment{}, session.ErrAttachmentNotFound
	}
	var attachment session.Attachment
	var filename, mimeType string
	var size int64
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT app_id,attachment_id,session_id,message_id,uploader_user_id,filename,mime_type,size,blob_id,created_at FROM attachments WHERE app_id=? AND session_id=? AND attachment_id=?`,
		appID, sessionID, attachmentID,
	).Scan(
		&attachment.AppID, &attachment.AttachmentID, &attachment.SessionID, &attachment.MessageID, &attachment.UploaderUserID,
		&filename, &mimeType, &size, &attachment.Ref.BlobID, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Attachment{}, session.ErrAttachmentNotFound
	}
	if err != nil {
		return session.Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	attachment.Ref = session.AttachmentRef{Filename: filename, MimeType: mimeType, Size: size, BlobID: attachment.Ref.BlobID}
	attachment.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return session.Attachment{}, fmt.Errorf("parse attachment created_at: %w", err)
	}
	return attachment, nil
}

// readSession 读取会话及其成员与平台绑定。querier 可以是数据库或事务。
func readSession(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, appID, sessionID string) (session.Session, error) {
	var sess session.Session
	var sessionType, createdAt, updatedAt string
	err := querier.QueryRowContext(ctx, `SELECT app_id,session_id,type,created_at,updated_at FROM sessions WHERE app_id=? AND session_id=?`,
		appID, sessionID,
	).Scan(&sess.AppID, &sess.SessionID, &sessionType, &createdAt, &updatedAt)
	if err != nil {
		return session.Session{}, err
	}
	sess.Type = sessionType
	if sess.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return session.Session{}, fmt.Errorf("parse session created_at: %w", err)
	}
	if sess.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return session.Session{}, fmt.Errorf("parse session updated_at: %w", err)
	}
	rows, err := querier.QueryContext(ctx, `SELECT user_id,role,joined_at FROM session_members WHERE app_id=? AND session_id=? ORDER BY user_id`,
		appID, sessionID,
	)
	if err != nil {
		return session.Session{}, fmt.Errorf("query session members: %w", err)
	}
	for rows.Next() {
		var member session.Member
		var joinedAt string
		if err := rows.Scan(&member.UserID, &member.Role, &joinedAt); err != nil {
			rows.Close()
			return session.Session{}, fmt.Errorf("scan session member: %w", err)
		}
		if member.JoinedAt, err = time.Parse(time.RFC3339Nano, joinedAt); err != nil {
			rows.Close()
			return session.Session{}, fmt.Errorf("parse session member joined_at: %w", err)
		}
		sess.Members = append(sess.Members, member)
	}
	if err := rows.Close(); err != nil {
		return session.Session{}, fmt.Errorf("close session member rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return session.Session{}, fmt.Errorf("iterate session members: %w", err)
	}
	bindingRows, err := querier.QueryContext(ctx, `SELECT platform,platform_id,bound_at FROM session_bindings WHERE app_id=? AND session_id=? ORDER BY platform,platform_id`,
		appID, sessionID,
	)
	if err != nil {
		return session.Session{}, fmt.Errorf("query session bindings: %w", err)
	}
	for bindingRows.Next() {
		var binding session.PlatformBinding
		var boundAt string
		if err := bindingRows.Scan(&binding.Platform, &binding.PlatformID, &boundAt); err != nil {
			bindingRows.Close()
			return session.Session{}, fmt.Errorf("scan session binding: %w", err)
		}
		if binding.BoundAt, err = time.Parse(time.RFC3339Nano, boundAt); err != nil {
			bindingRows.Close()
			return session.Session{}, fmt.Errorf("parse session binding bound_at: %w", err)
		}
		sess.PlatformBindings = append(sess.PlatformBindings, binding)
	}
	if err := bindingRows.Close(); err != nil {
		return session.Session{}, fmt.Errorf("close session binding rows: %w", err)
	}
	if err := bindingRows.Err(); err != nil {
		return session.Session{}, fmt.Errorf("iterate session bindings: %w", err)
	}
	return sess, nil
}

// sessionsEqual 比较两个会话是否等价（成员与平台绑定按集合比较，与顺序无关）。
func sessionsEqual(a, b session.Session) bool {
	if a.AppID != b.AppID || a.SessionID != b.SessionID || a.Type != b.Type ||
		!a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) ||
		len(a.Members) != len(b.Members) || len(a.PlatformBindings) != len(b.PlatformBindings) {
		return false
	}
	members := make(map[string]int, len(a.Members))
	for _, member := range a.Members {
		members[sessionIdentityKey(member)]++
	}
	for _, member := range b.Members {
		if members[sessionIdentityKey(member)] == 0 {
			return false
		}
		members[sessionIdentityKey(member)]--
	}
	bindings := make(map[string]int, len(a.PlatformBindings))
	for _, binding := range a.PlatformBindings {
		bindings[binding.Platform+"\x1f"+binding.PlatformID+"\x1f"+binding.BoundAt.UTC().Format(time.RFC3339Nano)]++
	}
	for _, binding := range b.PlatformBindings {
		key := binding.Platform + "\x1f" + binding.PlatformID + "\x1f" + binding.BoundAt.UTC().Format(time.RFC3339Nano)
		if bindings[key] == 0 {
			return false
		}
		bindings[key]--
	}
	return true
}

func sessionIdentityKey(member session.Member) string {
	return member.UserID + "\x1f" + member.Role + "\x1f" + member.JoinedAt.UTC().Format(time.RFC3339Nano)
}

// readMessage 读取消息元数据与正文（含已删除行，供去重回放与冲突判定）。
func readMessage(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, appID, messageID string) (session.Message, []byte, error) {
	return scanMessageRow(querier.QueryRowContext(ctx, messageSelect+`WHERE app_id=? AND message_id=?`, appID, messageID))
}

// readMessageByPlatform 按平台去重键读取既有消息（含已删除行）。
func readMessageByPlatform(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, appID, platformMessageID string) (session.Message, []byte, error) {
	return scanMessageRow(querier.QueryRowContext(ctx, messageSelect+`WHERE app_id=? AND platform_message_id=?`, appID, platformMessageID))
}

func scanMessageRow(scanner interface{ Scan(...any) error }) (session.Message, []byte, error) {
	var message session.Message
	var mode, blobID string
	var size int64
	var content []byte
	var createdAt string
	var editedAt, deletedAt sql.NullString
	if err := scanner.Scan(
		&message.AppID, &message.MessageID, &message.SessionID, &message.SenderUserID, &message.Type,
		&mode, &blobID, &size, &content, &message.ReplyTo, &message.PlatformMessageID,
		&createdAt, &editedAt, &deletedAt,
	); err != nil {
		return session.Message{}, nil, err
	}
	message.ContentRef = session.ContentRef{Mode: mode, BlobID: blobID, Size: size}
	var err error
	if message.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return session.Message{}, nil, fmt.Errorf("parse message created_at: %w", err)
	}
	if message.EditedAt, err = parseOptionalTime(editedAt); err != nil {
		return session.Message{}, nil, fmt.Errorf("parse message edited_at: %w", err)
	}
	if message.DeletedAt, err = parseOptionalTime(deletedAt); err != nil {
		return session.Message{}, nil, fmt.Errorf("parse message deleted_at: %w", err)
	}
	return message, content, nil
}

func scanMessageMetadata(scanner interface{ Scan(...any) error }) (session.Message, error) {
	var message session.Message
	var mode, blobID string
	var size int64
	var createdAt string
	var editedAt, deletedAt sql.NullString
	if err := scanner.Scan(
		&message.AppID, &message.MessageID, &message.SessionID, &message.SenderUserID, &message.Type,
		&mode, &blobID, &size, &message.ReplyTo, &message.PlatformMessageID,
		&createdAt, &editedAt, &deletedAt,
	); err != nil {
		return session.Message{}, err
	}
	message.ContentRef = session.ContentRef{Mode: mode, BlobID: blobID, Size: size}
	var err error
	if message.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return session.Message{}, fmt.Errorf("parse message created_at: %w", err)
	}
	if message.EditedAt, err = parseOptionalTime(editedAt); err != nil {
		return session.Message{}, fmt.Errorf("parse message edited_at: %w", err)
	}
	if message.DeletedAt, err = parseOptionalTime(deletedAt); err != nil {
		return session.Message{}, fmt.Errorf("parse message deleted_at: %w", err)
	}
	return message, nil
}

// messageIdentityEqual 判定去重回放：元数据与正文均一致才视为同一条消息的重复投递。
func messageIdentityEqual(existing session.Message, existingContent []byte, incoming session.Message, incomingContent []byte) bool {
	return existing.SessionID == incoming.SessionID &&
		existing.SenderUserID == incoming.SenderUserID &&
		existing.Type == incoming.Type &&
		existing.ReplyTo == incoming.ReplyTo &&
		existing.PlatformMessageID == incoming.PlatformMessageID &&
		existing.ContentRef.Mode == incoming.ContentRef.Mode &&
		existing.ContentRef.BlobID == incoming.ContentRef.BlobID &&
		existing.ContentRef.Size == incoming.ContentRef.Size &&
		(existing.ContentRef.Mode != session.ContentModeInline || bytes.Equal(existingContent, incomingContent))
}

// isForeignKeyConstraint 判定 SQLITE_CONSTRAINT_FOREIGNKEY（扩展码 787）。
func isForeignKeyConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code() == 787
}
