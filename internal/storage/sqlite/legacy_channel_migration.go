package sqlite

// 迁移 20–21 是已发布的旧配置版本。当前 App 配置已经由迁移 26 收敛为
// ExecutorID + opaque ExecutorConfig；这里仅保留旧数据库升级所需的历史步骤，
// 当前 Store 不读取 channel_prompts。
func init() {
	registerMigration(20, `
ALTER TABLE runs ADD COLUMN channel TEXT NOT NULL DEFAULT '';
ALTER TABLE app_config_revisions ADD COLUMN channel_prompts TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(channel_prompts));
`)
}

func init() {
	registerMigration(21, `
UPDATE app_config_heads SET updated_at = updated_at WHERE 1 = 0;
`)
}
