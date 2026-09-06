package sqlite

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

// 编译期断言：sqlite.Store 必须实现 Echo 编排的各个领域端口。
var (
	_ kernelecho.EchoCreationStore    = (*Store)(nil)
	_ kernelecho.RunExecutionStore    = (*Store)(nil)
	_ kernelecho.RunRecoveryStore     = (*Store)(nil)
	_ kernelecho.RunCancellationStore = (*Store)(nil)
	_ kernelecho.EchoEventStore       = (*Store)(nil)
	_ kernelecho.CapabilityAuditStore = (*Store)(nil)
)

func init() {
	// 迁移 19：上下文装配列。Run 持久化会话上下文（session_id/user_id/message_id）
	// 供重试恢复时按原会话重新装配；context_digest 固化确定性快照摘要，
	// context_sources 固化来源版本（配置修订/历史版本/Capability 投影，不含正文）。
	// 已有行取默认值（空会话、未装配），新 Run 在创建时由装配链路填充会话字段，
	// context_digest 在执行开始时由 SetRunContext 一次性设置。
	registerMigration(19, `
ALTER TABLE runs ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN context_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN context_sources TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(context_sources));
`)
}

// SetRunContext 固化 Run 的上下文摘要与来源版本。摘要必须是 64 位十六进制
// （sha256），来源版本必须是合法 JSON；同一 Run 每次执行只允许设置一次
// （context_digest 已非空时视为非法转换）。
func (s *Store) SetRunContext(ctx context.Context, run kernelecho.RunRecord, digest string, sources json.RawMessage) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "set_run_context", started, resultErr) }()
	if run.AppID == "" || run.ID == "" || run.EchoID == "" || run.LeaseToken == "" ||
		run.Status != kernelecho.RunStatusRunning || len(sources) == 0 || !json.Valid(sources) {
		return kernelecho.ErrInvalidRunRecord
	}
	decoded, decodeErr := hex.DecodeString(digest)
	if len(digest) != 64 || decodeErr != nil || len(decoded) != 32 {
		return kernelecho.ErrInvalidRunRecord
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE runs
SET context_digest=?,context_sources=?
WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=? AND context_digest=''`,
		digest, string(sources), run.AppID, run.ID, run.EchoID,
		kernelecho.RunStatusRunning, run.LeaseToken,
	)
	if err != nil {
		return fmt.Errorf("set Run context: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Run context update count: %w", err)
	}
	if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}
