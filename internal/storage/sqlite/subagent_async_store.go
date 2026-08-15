package sqlite

func init() {
	registerMigration(24, `
ALTER TABLE runs ADD COLUMN task_message TEXT NOT NULL DEFAULT '' CHECK(length(task_message)<=16000);
`)
}
