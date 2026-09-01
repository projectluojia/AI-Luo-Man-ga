package sqlite

// 迁移 23 是已发布数据库版本的一部分。当前内核不再提供 Prompt Service，
// 但必须保留原始迁移以维持旧数据库的连续升级历史；该表不参与当前运行路径。
func init() {
	registerMigration(23, `
CREATE TABLE user_prompt_settings (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  basic_style TEXT NOT NULL CHECK(length(basic_style) BETWEEN 1 AND 128),
  extra_trait_levels TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(extra_trait_levels) AND json_type(extra_trait_levels)='object'),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (app_id, user_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE INDEX user_prompt_settings_user_idx ON user_prompt_settings(user_id);
`)
}
