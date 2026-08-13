package sqlite

// 迁移 20：渠道化系统提示。app_config_revisions 增加渠道提示映射（JSON），
// runs 增加渠道标识，使同一 App 可按渠道（web/qq 群聊/qq 私聊）装配不同的
// 系统提示段。已有行取默认值（无渠道提示、空渠道），行为与迁移前一致。
// 摘要兼容：App 配置修订的摘要输入在渠道提示为空时省略该字段，旧行读回
// 重新归一化后摘要仍与迁移前一致（历史 Run 恢复不受影响）。
func init() {
	registerMigration(20, `
ALTER TABLE runs ADD COLUMN channel TEXT NOT NULL DEFAULT '';
ALTER TABLE app_config_revisions ADD COLUMN channel_prompts TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(channel_prompts));
`)
}

// 迁移 21：存量 App 配置重新播种。清空 app_config_heads（当前指针），使启动
// Ensure 以含渠道提示的新种子重新播种当前配置。历史修订（app_config_revisions）
// 保留供历史 Run 恢复；当前配置可由种子重建，属可恢复的破坏性迁移（备份由
// 种子本身承担）。与迁移 20 分离，避免改写已应用的迁移。
func init() {
	registerMigration(21, `
DELETE FROM app_config_heads;
`)
}
