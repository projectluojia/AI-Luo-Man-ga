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

// 迁移 21：存量 App 配置保留（非破坏性）。
// 渠道化系统提示由迁移 20 提供 channel_prompts 列，存量修订读回为空映射（'{}'），
// 行为与迁移前一致；不再清空 app_config_heads——删除当前指针会丢失控制面调优的
// 当前配置，且破坏"迁移不得毁坏当前权威状态"的规则。需要渠道提示的部署经配置
// CAS 或全新播种获得，main.go 种子不覆盖既有配置。
func init() {
	registerMigration(21, `
UPDATE app_config_heads SET updated_at = updated_at WHERE 1 = 0;
`)
}
