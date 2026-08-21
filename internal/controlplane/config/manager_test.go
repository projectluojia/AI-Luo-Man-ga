package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
)

func TestManagerStartsInSetupModeAndPersistsSecretsPrivately(t *testing.T) {
	root := t.TempDir()
	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready := manager.CurrentResolved(); ready {
		t.Fatal("unconfigured manager reported ready")
	}
	snapshot, err := manager.Save(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.Revision != 1 || !snapshot.ModelAPIKeyConfigured || !snapshot.QQWSTokenConfigured {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	settingsContent, err := os.ReadFile(filepath.Join(root, "ailuo-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(settingsContent), "model-secret") || strings.Contains(string(settingsContent), "qq-secret") {
		t.Fatal("secret leaked into ordinary settings file")
	}
	for _, name := range []string{"model-api-key", "qq-ws-token"} {
		info, err := os.Stat(filepath.Join(root, "secrets", name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("secret file %s permissions=%o", name, info.Mode().Perm())
		}
	}
	reloaded, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ready := reloaded.CurrentResolved()
	if !ready || resolved.Settings.Model != "test-model" || resolved.Settings.QQBotID != "2647414417" {
		t.Fatalf("resolved=%+v ready=%v", resolved.Settings, ready)
	}
	if len(resolved.Settings.QQQuickReplies) != 1 || resolved.Settings.QQQuickReplies[0] != (QQQuickReply{Trigger: "ping", Reply: "pong"}) {
		t.Fatalf("quick replies=%#v", resolved.Settings.QQQuickReplies)
	}
	if len(resolved.Settings.QQPokeReplies) != 1 || resolved.Settings.QQPokeReplies[0] != "在呢" {
		t.Fatalf("poke replies=%#v", resolved.Settings.QQPokeReplies)
	}
}

func TestManagerRejectsDuplicateQuickReplyTriggers(t *testing.T) {
	manager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.QQQuickReplies = []QQQuickReply{{Trigger: "ping", Reply: "one"}, {Trigger: " ping ", Reply: "two"}}
	if _, err := manager.Save(input); err != ErrInvalid {
		t.Fatalf("duplicate quick reply error=%v", err)
	}
}

func TestManagerLoadsLegacySettingsWithDefaultPokeReplies(t *testing.T) {
	root := t.TempDir()
	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Save(validInput()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "ailuo-settings.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(content), `,"qq_quick_replies":[{"trigger":"ping","reply":"pong"}],"qq_poke_replies":["在呢"]`, "", 1)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshot().Settings.QQPokeReplies) == 0 {
		t.Fatal("legacy settings did not receive default poke replies")
	}
}

func TestManagerUsesRevisionCASAndPreservesBlankSecrets(t *testing.T) {
	manager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Save(validInput())
	if err != nil {
		t.Fatal(err)
	}
	conflict := validInput()
	conflict.Revision = 0
	if _, err := manager.Save(conflict); err != ErrConflict {
		t.Fatalf("conflict error=%v", err)
	}
	second := validInput()
	second.Revision = first.Settings.Revision
	second.ModelAPIKey = ""
	second.QQWSToken = ""
	second.Model = "next-model"
	snapshot, err := manager.Save(second)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.Revision != 2 || snapshot.Settings.Model != "next-model" || !snapshot.ModelAPIKeyConfigured || !snapshot.QQWSTokenConfigured {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func validInput() SaveInput {
	return SaveInput{
		Model: "test-model", ModelBaseURL: "https://models.example.test/v1", ModelAPIKey: "model-secret",
		ModelRequestTimeoutSeconds: 30, ModelReadinessTimeoutSeconds: 3, ModelMaxRetries: 2,
		ModelRetryBaseSeconds: 0.25, ModelRetryMaxSeconds: 2, ModelRequestsPerMinute: 60, ModelMaxConcurrency: 4,
		QQEnabled: true, QQWSURL: "ws://127.0.0.1:3001", QQWSToken: "qq-secret", QQBotID: "2647414417",
		QQAllowedGroupIDs: []string{"123456"}, QQAllowedPrivateUserIDs: []string{"654321"},
		QQQuickReplies: []QQQuickReply{{Trigger: " ping ", Reply: " pong "}}, QQPokeReplies: []string{" 在呢 "},
	}
}

func TestManagerPersistsPromptCatalogAndDefaultsLegacySettings(t *testing.T) {
	root := t.TempDir()
	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.PromptCatalog = promptcatalog.Default()
	input.PromptCatalog.BasicStyles[0].Text = "自定义默认风格"
	input.BaseSystemPrompt = "自定义基础系统提示"
	input.ChannelPrompts = map[string]string{"web": "自定义 web 渠道提示", "qq_group": "自定义群渠道提示", "qq_private": "自定义私聊渠道提示"}
	snapshot, err := manager.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.PromptCatalog.BasicStyles[0].Text != "自定义默认风格" || snapshot.Settings.BaseSystemPrompt != "自定义基础系统提示" ||
		snapshot.Settings.ChannelPrompts["web"] != "自定义 web 渠道提示" {
		t.Fatalf("prompt catalog=%#v base=%q channels=%#v", snapshot.Settings.PromptCatalog.BasicStyles[0], snapshot.Settings.BaseSystemPrompt, snapshot.Settings.ChannelPrompts)
	}
	reloaded, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ready := reloaded.CurrentResolved()
	gotStyle := ""
	if len(resolved.Settings.PromptCatalog.BasicStyles) > 0 {
		gotStyle = resolved.Settings.PromptCatalog.BasicStyles[0].Text
	}
	if !ready || gotStyle != "自定义默认风格" || resolved.Settings.BaseSystemPrompt != "自定义基础系统提示" ||
		resolved.Settings.ChannelPrompts["web"] != "自定义 web 渠道提示" {
		t.Fatalf("reloaded prompt catalog style=%q styles=%d base=%q channels=%#v ready=%v", gotStyle, len(resolved.Settings.PromptCatalog.BasicStyles), resolved.Settings.BaseSystemPrompt, resolved.Settings.ChannelPrompts, ready)
	}
	// 旧配置文件没有 prompt_catalog 字段：加载时自动补默认目录。
	legacy := SaveInput{
		Model: "legacy-model", ModelAPIKey: "legacy-key",
		ModelRequestTimeoutSeconds: 30, ModelReadinessTimeoutSeconds: 3,
		ModelMaxRetries: 2, ModelRetryBaseSeconds: 0.25, ModelRetryMaxSeconds: 2,
		ModelRequestsPerMinute: 60, ModelMaxConcurrency: 4,
	}
	legacyManager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacySnapshot, err := legacyManager.Save(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacySnapshot.Settings.PromptCatalog.BasicStyles) != len(promptcatalog.Default().BasicStyles) {
		t.Fatalf("legacy prompt catalog=%#v", legacySnapshot.Settings.PromptCatalog)
	}
	if legacySnapshot.Settings.BaseSystemPrompt != promptcatalog.DefaultBaseSystemPrompt {
		t.Fatal("legacy settings did not receive default base system prompt")
	}
	if len(legacySnapshot.Settings.ChannelPrompts) != len(promptcatalog.DefaultChannelPrompts()) {
		t.Fatal("legacy settings did not receive default channel prompts")
	}
}

func TestManagerRejectsInvalidPromptCatalog(t *testing.T) {
	manager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.PromptCatalog = promptcatalog.Default()
	input.PromptCatalog.BasicStyles[0].Key = "changed"
	if _, err := manager.Save(input); err != ErrInvalid {
		t.Fatalf("invalid prompt catalog error=%v", err)
	}
}

func TestManagerPersistsRuntimeSettingsAndDefaultsLegacyValues(t *testing.T) {
	root := t.TempDir()
	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.AgentRun = AgentRunSettings{
		Timezone: "Asia/Tokyo", MaxSteps: 12, MaxToolCalls: 16, MaxInputTokens: 20000,
		MaxOutputTokens: 6000, MaxTotalTokens: 26000, MaxOutputBytes: 32768, MaxChildRuns: 3,
	}
	input.Orchestration = OrchestrationSettings{RunTimeoutSeconds: 120, MaxRunAttempts: 4, QueueCapacity: 256, MaxCallDepth: 20}
	input.ContextAssembly = ContextAssemblySettings{MaxMessages: 30, MaxCharsPerMsg: 3000, MaxTotalChars: 20000, MaxPromptBytes: 20000}
	input.Scheduler = SchedulerSettings{Workers: 6, PollMs: 400, BatchSize: 40}
	input.QQConnection = QQConnectionSettings{DialTimeoutSeconds: 12, ReconnectDelaySeconds: 8, RunTimeoutSeconds: 240, ManagerStopTimeoutSeconds: 9}
	input.AgentProcess = AgentProcessSettings{DialTimeoutSeconds: 20, StopGraceSeconds: 8, TerminateGraceSeconds: 3}
	input.Governance = GovernanceSettings{ConfirmationSweepSeconds: 600}
	snapshot, err := manager.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	settings := reloaded.Snapshot().Settings
	if settings.AgentRun.Timezone != "Asia/Tokyo" || settings.AgentRun.MaxChildRuns != 3 ||
		settings.Orchestration.RunTimeoutSeconds != 120 || settings.ContextAssembly.MaxMessages != 30 ||
		settings.Scheduler.Workers != 6 || settings.QQConnection.ReconnectDelaySeconds != 8 || settings.QQConnection.ManagerStopTimeoutSeconds != 9 ||
		settings.AgentProcess.StopGraceSeconds != 8 || settings.Governance.ConfirmationSweepSeconds != 600 {
		t.Fatalf("runtime settings=%+v", settings)
	}
	if snapshot.Settings.AgentRun.MaxSteps != 12 {
		t.Fatalf("snapshot agent run=%+v", snapshot.Settings.AgentRun)
	}
	// 旧配置没有嵌套运行时设置：自动补默认值。
	legacy, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacySnapshot, err := legacy.Save(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if legacySnapshot.Settings.AgentRun.MaxChildRuns != 4 ||
		legacySnapshot.Settings.Orchestration.QueueCapacity != 128 ||
		legacySnapshot.Settings.ContextAssembly.MaxMessages != 20 {
		t.Fatalf("legacy runtime defaults=%+v", legacySnapshot.Settings)
	}
}
