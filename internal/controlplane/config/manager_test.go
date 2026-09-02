package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagerStartsInSetupModeAndPersistsQQSecretPrivately(t *testing.T) {
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
	if snapshot.Settings.Revision != 1 || !snapshot.QQWSTokenConfigured {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	settingsContent, err := os.ReadFile(filepath.Join(root, "ailuo-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(settingsContent), "qq-secret") {
		t.Fatal("secret leaked into ordinary settings file")
	}
	for _, name := range []string{"qq-ws-token"} {
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
	if !ready || resolved.Settings.AppID != "test-app" || resolved.Settings.ExecutorID != "executor.test" || resolved.Settings.QQBotID != "2647414417" {
		t.Fatalf("resolved=%+v ready=%v", resolved.Settings, ready)
	}
	if len(resolved.Settings.QQQuickReplies) != 1 || resolved.Settings.QQQuickReplies[0] != (QQQuickReply{Trigger: "ping", Reply: "pong"}) {
		t.Fatalf("quick replies=%#v", resolved.Settings.QQQuickReplies)
	}
	if len(resolved.Settings.QQPokeReplies) != 1 || resolved.Settings.QQPokeReplies[0] != "在呢" {
		t.Fatalf("poke replies=%#v", resolved.Settings.QQPokeReplies)
	}
}

func TestManagerRequiresFreshSaveForLegacyProviderAndProcessFields(t *testing.T) {
	root := t.TempDir()
	settings, err := normalize(validInput())
	if err != nil {
		t.Fatal(err)
	}
	settings.Revision = 1
	settings.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	process, err := json.Marshal(settings.RuntimeProcess)
	if err != nil {
		t.Fatal(err)
	}
	delete(legacy, "runtime_process")
	legacy["agent_process"] = process
	for key, value := range map[string]any{
		"model_base_url":                  "http://127.0.0.1:8081/v1",
		"model_request_timeout_seconds":   30,
		"model_readiness_timeout_seconds": 3,
		"model_max_retries":               2,
		"model_retry_base_seconds":        0.25,
		"model_retry_max_seconds":         2,
		"model_requests_per_minute":       60,
		"model_max_concurrency":           4,
	} {
		legacy[key], err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyBytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ailuo-settings.json"), legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready := manager.CurrentResolved(); ready {
		t.Fatal("legacy settings were exposed as a resolved configuration")
	}
	if runtime := manager.Snapshot().Runtime; runtime.State != "setup_required" {
		t.Fatalf("runtime=%+v, want setup_required", runtime)
	}
	if _, err := manager.Save(validInput()); err != nil {
		t.Fatalf("fresh save after legacy settings: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(root, "ailuo-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "model_base_url") || strings.Contains(string(current), "agent_process") {
		t.Fatal("fresh settings retained legacy fields")
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

func TestManagerRejectsIncompleteSettings(t *testing.T) {
	manager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.ExecutorConfig = json.RawMessage(`{`)
	input.Execution = ExecutionSettings{}
	if _, err := manager.Save(input); err != ErrInvalid {
		t.Fatalf("incomplete settings error=%v", err)
	}
}

func TestManagerRejectsMissingAppID(t *testing.T) {
	manager, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.AppID = ""
	if _, err := manager.Save(input); err != ErrInvalid {
		t.Fatalf("missing AppID error=%v", err)
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
	second.QQWSToken = ""
	second.ExecutorID = "next.executor"
	snapshot, err := manager.Save(second)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.Revision != 2 || snapshot.Settings.ExecutorID != "next.executor" || !snapshot.QQWSTokenConfigured {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func validInput() SaveInput {
	defaults := DefaultSettings()
	return SaveInput{
		AppID: "test-app", ExecutorID: "executor.test", ExecutorConfig: defaults.ExecutorConfig, ExecutorTimeoutSeconds: 30,
		QQEnabled: true, QQWSURL: "ws://127.0.0.1:3001", QQWSToken: "qq-secret", QQBotID: "2647414417",
		QQAllowedGroupIDs: []string{"123456"}, QQAllowedPrivateUserIDs: []string{"654321"},
		QQQuickReplies: []QQQuickReply{{Trigger: " ping ", Reply: " pong "}}, QQPokeReplies: []string{" 在呢 "},
		Execution: defaults.Execution, Orchestration: defaults.Orchestration, ContextAssembly: defaults.ContextAssembly,
		Scheduler: defaults.Scheduler, QQConnection: defaults.QQConnection, RuntimeProcess: defaults.RuntimeProcess,
		Governance: defaults.Governance,
	}
}

func TestManagerPersistsRuntimeSettings(t *testing.T) {
	root := t.TempDir()
	manager, err := NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.Execution = ExecutionSettings{
		MaxSteps: 12, MaxCapabilityCalls: 16, MaxExecutionUnits: 26000,
		MaxOutputBytes: 32768, MaxCostMicrousd: 0,
	}
	input.Orchestration = OrchestrationSettings{RunTimeoutSeconds: 120, MaxRunAttempts: 4, QueueCapacity: 256, MaxCallDepth: 20}
	input.ContextAssembly = ContextAssemblySettings{MaxMessages: 30, MaxCharsPerMsg: 3000, MaxTotalChars: 20000, MaxContextBytes: 20000}
	input.Scheduler = SchedulerSettings{Workers: 6, PollMs: 400, BatchSize: 40}
	input.QQConnection = QQConnectionSettings{DialTimeoutSeconds: 12, ReconnectDelaySeconds: 8, RunTimeoutSeconds: 240, ManagerStopTimeoutSeconds: 9}
	input.RuntimeProcess = RuntimeProcessSettings{DialTimeoutSeconds: 20, StopGraceSeconds: 8, TerminateGraceSeconds: 3}
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
	if settings.Orchestration.RunTimeoutSeconds != 120 || settings.ContextAssembly.MaxMessages != 30 ||
		settings.Scheduler.Workers != 6 || settings.QQConnection.ReconnectDelaySeconds != 8 || settings.QQConnection.ManagerStopTimeoutSeconds != 9 ||
		settings.RuntimeProcess.StopGraceSeconds != 8 || settings.Governance.ConfirmationSweepSeconds != 600 {
		t.Fatalf("runtime settings=%+v", settings)
	}
	if snapshot.Settings.Execution.MaxSteps != 12 {
		t.Fatalf("snapshot executor run=%+v", snapshot.Settings.Execution)
	}
}
