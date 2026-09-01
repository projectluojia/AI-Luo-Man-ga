package sqlite

// 迁移 27：当前表中的错误分类统一使用 Executor 语义。旧版本已经把
// agent_* 错误写入 Echo/Run 的数据列；它们不再属于当前运行契约，升级时一次性
// 归一化，历史迁移文件仍保留原始输入以支持从旧数据库升级。
func init() {
	registerMigration(27, `
UPDATE echoes
SET error_code=CASE error_code
  WHEN 'agent_unavailable' THEN 'executor_unavailable'
  WHEN 'agent_start_failed' THEN 'executor_unavailable'
  WHEN 'agent_stream_failed' THEN 'executor_unavailable'
  WHEN 'agent_run_failed' THEN 'executor_failed'
  ELSE error_code
END,
error_message=CASE error_code
  WHEN 'agent_unavailable' THEN '执行者暂时不可用'
  WHEN 'agent_start_failed' THEN '执行者暂时不可用'
  WHEN 'agent_stream_failed' THEN '执行者暂时不可用'
  WHEN 'agent_run_failed' THEN '执行者 Run 执行失败'
  WHEN 'protocol_violation' THEN '执行者协议响应无效'
  ELSE error_message
END
WHERE error_code IN ('agent_unavailable','agent_start_failed','agent_stream_failed','agent_run_failed','protocol_violation');

UPDATE runs
SET error_code=CASE error_code
  WHEN 'agent_unavailable' THEN 'executor_unavailable'
  WHEN 'agent_start_failed' THEN 'executor_unavailable'
  WHEN 'agent_stream_failed' THEN 'executor_unavailable'
  WHEN 'agent_run_failed' THEN 'executor_failed'
  ELSE error_code
END,
error_message=CASE error_code
  WHEN 'agent_unavailable' THEN '执行者暂时不可用'
  WHEN 'agent_start_failed' THEN '执行者暂时不可用'
  WHEN 'agent_stream_failed' THEN '执行者暂时不可用'
  WHEN 'agent_run_failed' THEN '执行者 Run 执行失败'
  WHEN 'protocol_violation' THEN '执行者协议响应无效'
  ELSE error_message
END
WHERE error_code IN ('agent_unavailable','agent_start_failed','agent_stream_failed','agent_run_failed','protocol_violation');
`)
}
