package sqlite

// 迁移 26：将 Run 与 App 配置中的能力调用预算改为中性命名。
// 历史迁移保留原始 SQL，当前 Schema 通过前向迁移完成一次明确的列名变更。
func init() {
	registerMigration(26, `
ALTER TABLE runs RENAME COLUMN max_tool_calls TO max_capability_calls;
ALTER TABLE app_config_revisions RENAME COLUMN max_tool_calls TO max_capability_calls;
`)
}
