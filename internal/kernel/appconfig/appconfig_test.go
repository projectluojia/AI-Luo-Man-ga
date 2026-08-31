package appconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

func TestNormalizeProducesCanonicalContentRevision(t *testing.T) {
	config := validConfig()
	config.EnabledCapabilities = []string{"campus.bus.stops.search", "campus.bus.routes.list"}
	first, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	config.EnabledCapabilities = []string{"campus.bus.routes.list", "campus.bus.stops.search"}
	second, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != second.Revision || first.Revision == "" ||
		first.EnabledCapabilities[0] != "campus.bus.routes.list" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	second.SystemPrompt = "不同配置"
	second, err = appconfig.Normalize(second)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == second.Revision {
		t.Fatal("different configuration reused a revision")
	}
	config = validConfig()
	config.EnabledCapabilities = nil
	config.PermissionScope = []string{"institution:bus.read"}
	empty, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EnabledCapabilities == nil || empty.PermissionScope == nil {
		t.Fatalf("empty collections were not canonical JSON arrays: %#v", empty)
	}
}

func TestNormalizeRejectsUnsafeOrUnboundedConfiguration(t *testing.T) {
	tests := []appconfig.Config{
		func() appconfig.Config { value := validConfig(); value.AppID = "../other"; return value }(),
		func() appconfig.Config { value := validConfig(); value.Model = "model\nsecret"; return value }(),
		func() appconfig.Config { value := validConfig(); value.SystemPrompt = "bad\x00prompt"; return value }(),
		func() appconfig.Config { value := validConfig(); value.SystemPrompt = " \n\t"; return value }(),
		func() appconfig.Config { value := validConfig(); value.Timezone = "unknown/zone"; return value }(),
		func() appconfig.Config { value := validConfig(); value.MaxSteps = 65; return value }(),
		func() appconfig.Config { value := validConfig(); value.MaxTotalTokens = 1; return value }(),
		func() appconfig.Config {
			value := validConfig()
			value.PermissionScope = []string{"bus.read", "bus.read"}
			return value
		}(),
		func() appconfig.Config {
			value := validConfig()
			value.PermissionScope = []string{strings.Repeat("a", 129)}
			return value
		}(),
		func() appconfig.Config {
			value := validConfig()
			value.ChannelPrompts = map[string]string{"Bad/Key": "提示"}
			return value
		}(),
		func() appconfig.Config {
			value := validConfig()
			value.ChannelPrompts = map[string]string{"qq_group": " \n\t"}
			return value
		}(),
		func() appconfig.Config {
			value := validConfig()
			value.ChannelPrompts = map[string]string{"qq_group": "bad\x00prompt"}
			return value
		}(),
	}
	for _, config := range tests {
		if _, err := appconfig.Normalize(config); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}
}

func TestChannelPromptsEnterRevisionDigest(t *testing.T) {
	config := validConfig()
	config.ChannelPrompts = map[string]string{"qq_group": "群聊规则", "web": "网页规则"}
	first, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	// 相同内容（map 顺序无关）必须产生相同修订。
	config.ChannelPrompts = map[string]string{"web": "网页规则", "qq_group": "群聊规则"}
	second, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != second.Revision {
		t.Fatalf("map 顺序不影响修订：%s != %s", first.Revision, second.Revision)
	}
	// 渠道提示变化必须改变修订（配置即契约）。
	config.ChannelPrompts = map[string]string{"qq_group": "不同的群聊规则", "web": "网页规则"}
	third, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == third.Revision {
		t.Fatal("渠道提示变化未改变修订")
	}
}

func TestVerifyRejectsMismatchedIdentityAndRevisionContent(t *testing.T) {
	config, err := appconfig.Normalize(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	if err := appconfig.VerifyCurrent(config, config.AppID); err != nil {
		t.Fatal(err)
	}
	withoutGeneration := config
	withoutGeneration.Generation = 0
	if err := appconfig.VerifyCurrent(withoutGeneration, config.AppID); err == nil {
		t.Fatal("current configuration without a generation passed verification")
	}
	if err := appconfig.Verify(config, config.AppID, config.Revision); err != nil {
		t.Fatal(err)
	}
	if err := appconfig.Verify(config, "other-app", config.Revision); err == nil {
		t.Fatal("mismatched App identity passed verification")
	}
	if err := appconfig.Verify(config, config.AppID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched expected revision passed verification")
	}
	config.Model = "tampered-model"
	if err := appconfig.Verify(config, config.AppID, config.Revision); err == nil {
		t.Fatal("tampered revision content passed verification")
	}
	snapshot := appconfig.Snapshot(config)
	snapshot.AppID = "other-app"
	if err := snapshot.Verify("campus-services"); err == nil {
		t.Fatal("mismatched policy snapshot passed verification")
	}
	snapshot = appconfig.PolicySnapshot{
		AppID: "campus-services", Revision: "static", Generation: 1, Enabled: true,
		EnabledCapabilities: []string{"z", "a"},
	}
	if err := snapshot.Verify("campus-services"); err == nil {
		t.Fatal("non-canonical policy snapshot passed verification")
	}
}

func validConfig() appconfig.Config {
	return appconfig.Config{
		AppID: "campus-services", Enabled: true, Model: "test-model",
		SystemPrompt: "系统提示", Timezone: "Asia/Shanghai",
		MaxSteps: 8, MaxToolCalls: 8, MaxInputTokens: 32768, MaxOutputTokens: 8192,
		MaxTotalTokens: 40960, MaxOutputBytes: 65536, ProviderTimeout: 30 * time.Second,
		EnabledCapabilities: []string{"campus.bus.routes.list"}, PermissionScope: []string{"bus.read"},
	}
}
