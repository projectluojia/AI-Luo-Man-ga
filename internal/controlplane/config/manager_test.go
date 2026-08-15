package config

import (
	"os"
	"path/filepath"
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
		if info.Mode().Perm()&0o077 != 0 {
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
	snapshot, err := manager.Save(input)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.PromptCatalog.BasicStyles[0].Text != "自定义默认风格" || snapshot.Settings.BaseSystemPrompt != "自定义基础系统提示" {
		t.Fatalf("prompt catalog=%#v base=%q", snapshot.Settings.PromptCatalog.BasicStyles[0], snapshot.Settings.BaseSystemPrompt)
	}
	reloaded, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, ready := reloaded.CurrentResolved(); !ready || resolved.Settings.PromptCatalog.BasicStyles[0].Text != "自定义默认风格" || resolved.Settings.BaseSystemPrompt != "自定义基础系统提示" {
		t.Fatalf("reloaded prompt catalog=%#v base=%q ready=%v", resolved.Settings.PromptCatalog.BasicStyles[0], resolved.Settings.BaseSystemPrompt, ready)
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
