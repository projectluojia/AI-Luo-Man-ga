package sqlite

// 迁移 24 是已发布数据库版本的一部分。它为历史 Run 保留任务正文列；
// 当前 Executor 通过 input_payload 接收输入，旧列只由前向升级读取。
func init() {
	registerMigration(24, `
ALTER TABLE runs ADD COLUMN task_message TEXT NOT NULL DEFAULT '' CHECK(length(task_message)<=16000);
`)
}
