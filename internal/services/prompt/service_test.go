package prompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
)

type memorySettingsStore struct {
	values map[string]Settings
}

func newMemorySettingsStore() *memorySettingsStore {
	return &memorySettingsStore{values: make(map[string]Settings)}
}

func (m *memorySettingsStore) key(appID, userID string) string { return appID + "/" + userID }

func (m *memorySettingsStore) GetPromptSettings(_ context.Context, appID, userID string) (Settings, error) {
	settings, ok := m.values[m.key(appID, userID)]
	if !ok {
		return Settings{}, ErrNotFound
	}
	return settings, nil
}

func (m *memorySettingsStore) SavePromptSettings(_ context.Context, appID string, settings Settings) error {
	settings, err := NormalizeSettings(settings)
	if err != nil {
		return err
	}
	m.values[m.key(appID, settings.UserID)] = settings
	return nil
}

func (m *memorySettingsStore) DeletePromptSettings(_ context.Context, appID, userID string) error {
	if _, ok := m.values[m.key(appID, userID)]; !ok {
		return ErrNotFound
	}
	delete(m.values, m.key(appID, userID))
	return nil
}

func TestRenderUsesV2DefaultsForUserWithoutSettings(t *testing.T) {
	service := NewService(promptcatalog.Default(), newMemorySettingsStore())
	prompt, err := service.RenderSystemPrompt(t.Context(), RenderRequest{
		AppID:            "campus-services",
		UserID:           "user-1",
		BaseSystemPrompt: "【基本人格】测试",
		Channel:          "web",
		ChannelPrompts:   map[string]string{"web": "【端介绍】web"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"【基本人格】测试",
		"【基本风格与语调】\n默认：保持珞樱原有人格基调",
		"【额外特征】",
		"- 理解用户当前处境",
		"【端介绍】web",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("渲染结果缺少 %q：\n%s", expected, prompt)
		}
	}
}

func TestRenderAppliesStoredUserPreferences(t *testing.T) {
	store := newMemorySettingsStore()
	service := NewService(promptcatalog.Default(), store)
	if err := store.SavePromptSettings(t.Context(), "campus-services", Settings{
		UserID:     "user-1",
		BasicStyle: "professional",
		ExtraTraitLevels: map[string]string{
			"emoji":          "enhanced",
			"headings_lists": "reduced",
		},
	}); err != nil {
		t.Fatal(err)
	}
	prompt, err := service.RenderSystemPrompt(t.Context(), RenderRequest{
		AppID:            "campus-services",
		UserID:           "user-1",
		BaseSystemPrompt: "【基本人格】测试",
		Channel:          "qq_group",
		ChannelPrompts:   map[string]string{"qq_group": "【端介绍】群"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "【基本风格与语调】\n专业可靠：") {
		t.Fatalf("未应用专业可靠风格：\n%s", prompt)
	}
	if !strings.Contains(prompt, "- 在轻松、亲近、庆祝、闲聊、鼓励或调侃场景中可以更频繁使用 emoji") {
		t.Fatalf("未应用表情符号增强：\n%s", prompt)
	}
	if !strings.Contains(prompt, "- 减少标题和列表") {
		t.Fatalf("未应用标题和列表减弱：\n%s", prompt)
	}
}

func TestRenderRequiresBasePromptAndSkipsUnknownChannel(t *testing.T) {
	service := NewService(promptcatalog.Default(), newMemorySettingsStore())
	if _, err := service.RenderSystemPrompt(t.Context(), RenderRequest{
		AppID: "campus-services", BaseSystemPrompt: "   ",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank base prompt error=%v", err)
	}
	prompt, err := service.RenderSystemPrompt(t.Context(), RenderRequest{
		AppID:            "campus-services",
		BaseSystemPrompt: "【基本人格】测试",
		Channel:          "qq_group",
		ChannelPrompts:   nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "【端介绍】") {
		t.Fatalf("未知渠道不应渲染渠道段：\n%s", prompt)
	}
}

func TestSetSettingsRejectsUnknownStyleOrTrait(t *testing.T) {
	service := NewService(promptcatalog.Default(), newMemorySettingsStore())
	if _, err := service.SetSettings(t.Context(), "campus-services", "user-1", "不存在", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown style error=%v", err)
	}
	if _, err := service.SetSettings(t.Context(), "campus-services", "user-1", "默认", map[string]string{"不存在": "增强"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown trait error=%v", err)
	}
	if _, err := service.SetSettings(t.Context(), "campus-services", "user-1", "默认", map[string]string{"emoji": "超强"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown level error=%v", err)
	}
}

func TestRegisterExposesPreferenceCapabilitiesOnly(t *testing.T) {
	service := NewService(promptcatalog.Default(), newMemorySettingsStore())
	reg := registry.New()
	if err := Register(reg, service); err != nil {
		t.Fatal(err)
	}
	for _, capabilityID := range []string{PreferenceGetID, PreferenceSetID, PreferenceResetID} {
		spec, _, err := reg.ResolveCapability(capabilityID)
		if err != nil {
			t.Fatalf("capability %s: %v", capabilityID, err)
		}
		if spec.ServiceID != ServiceID {
			t.Fatalf("capability %s service=%s", capabilityID, spec.ServiceID)
		}
	}
	if services := reg.Services(); len(services) != 1 || services[0].ID != ServiceID {
		t.Fatalf("services=%#v", services)
	}
}
