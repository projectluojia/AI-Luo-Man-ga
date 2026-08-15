package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	}
}
