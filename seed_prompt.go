package main

import "github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"

// 默认系统提示种子：迁移自 LuoYingRebuild 的 system_prompt_parts.py。
// 迁移范围与不迁移项见 internal/promptcatalog/base.go 与 defaults.go 注释。
// main.go 在 App 配置 Ensure/CAS 时使用这两个默认值；控制面可以覆盖基础提示，
// 因此这里只保留包级别名，避免 main 与共享默认目录各自维护一份正文。
const defaultSystemPrompt = promptcatalog.DefaultBaseSystemPrompt

var defaultChannelPrompts = promptcatalog.DefaultChannelPrompts()
