package promptcatalog

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultCatalogMatchesV2Shape(t *testing.T) {
	catalog := Default()
	if len(catalog.BasicStyles) != 7 || len(catalog.ExtraTraits) != 4 {
		t.Fatalf("catalog shape=%d/%d", len(catalog.BasicStyles), len(catalog.ExtraTraits))
	}
	if catalog.BasicStyles[0].Key != "default" || catalog.BasicStyles[0].Name != "默认" ||
		!strings.Contains(catalog.BasicStyles[0].Text, "保持珞樱原有人格基调") {
		t.Fatalf("default style=%#v", catalog.BasicStyles[0])
	}
	if catalog.ExtraTraits[0].Key != "considerate" || catalog.ExtraTraits[0].Name != "温和体贴" {
		t.Fatalf("first trait=%#v", catalog.ExtraTraits[0])
	}
	for _, trait := range catalog.ExtraTraits {
		if trait.Enhanced == "" || trait.Default == "" || trait.Reduced == "" {
			t.Fatalf("incomplete trait=%#v", trait)
		}
	}
}

func TestNormalizeZeroCatalogReturnsDefaults(t *testing.T) {
	normalized, err := Normalize(Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.BasicStyles) != len(Default().BasicStyles) {
		t.Fatalf("normalized=%#v", normalized)
	}
}

func TestNormalizeAllowsEditingTextButNotKeys(t *testing.T) {
	catalog := Default()
	catalog.BasicStyles[0].Text = "自定义默认风格"
	catalog.ExtraTraits[0].Default = "自定义默认档"
	normalized, err := Normalize(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.BasicStyles[0].Text != "自定义默认风格" || normalized.ExtraTraits[0].Default != "自定义默认档" {
		t.Fatalf("normalized=%#v", normalized)
	}
	changed := Default()
	changed.BasicStyles[0].Key = "changed"
	if _, err := Normalize(changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed key error=%v", err)
	}
}

func TestNormalizeTrimsText(t *testing.T) {
	catalog := Default()
	catalog.BasicStyles[0].Name = "  默认  "
	catalog.BasicStyles[0].Text = "  正文  "
	normalized, err := Normalize(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.BasicStyles[0].Name != "默认" || normalized.BasicStyles[0].Text != "正文" {
		t.Fatalf("normalized=%#v", normalized.BasicStyles[0])
	}
}

func TestDefaultBaseSystemPromptContainsV2Persona(t *testing.T) {
	if !strings.Contains(DefaultBaseSystemPrompt, "你是“珞樱”（Luoying）") ||
		!strings.Contains(DefaultBaseSystemPrompt, "武汉大学人工智能学院专属数字伙伴") ||
		!strings.Contains(DefaultBaseSystemPrompt, "你可以一次性调用多个工具来提升效率") {
		t.Fatalf("base prompt missing V2 persona: %q", DefaultBaseSystemPrompt)
	}
	if channels := DefaultChannelPrompts(); len(channels) != 3 || channels["web"] == "" || channels["qq_group"] == "" || channels["qq_private"] == "" {
		t.Fatalf("channels=%#v", channels)
	}
}

func TestNormalizeBaseSystemPrompt(t *testing.T) {
	normalized, err := NormalizeBaseSystemPrompt("  自定义基础提示  ")
	if err != nil {
		t.Fatal(err)
	}
	if normalized != "自定义基础提示" {
		t.Fatalf("normalized=%q", normalized)
	}
	if normalized, err = NormalizeBaseSystemPrompt(""); err != nil || normalized != DefaultBaseSystemPrompt {
		t.Fatalf("empty base normalized=%q err=%v", normalized, err)
	}
	if _, err := NormalizeBaseSystemPrompt("bad\x00prompt"); !errors.Is(err, ErrInvalidBasePrompt) {
		t.Fatalf("nul base error=%v", err)
	}
}

func TestNormalizeChannelPrompts(t *testing.T) {
	prompts, err := NormalizeChannelPrompts(map[string]string{
		"web":        " 自定义 web  ",
		"qq_group":   " 自定义群 ",
		"qq_private": " 自定义私聊 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prompts["web"] != "自定义 web" || prompts["qq_group"] != "自定义群" || prompts["qq_private"] != "自定义私聊" {
		t.Fatalf("prompts=%#v", prompts)
	}
	if prompts, err = NormalizeChannelPrompts(nil); err != nil || len(prompts) != len(DefaultChannelPrompts()) {
		t.Fatalf("empty prompts=%#v err=%v", prompts, err)
	}
	if _, err := NormalizeChannelPrompts(map[string]string{"web": "x", "unknown": "y"}); !errors.Is(err, ErrInvalidChannelPrompts) {
		t.Fatalf("unknown channel error=%v", err)
	}
	if _, err := NormalizeChannelPrompts(map[string]string{"web": "x", "qq_group": "y"}); !errors.Is(err, ErrInvalidChannelPrompts) {
		t.Fatalf("incomplete channel error=%v", err)
	}
}
