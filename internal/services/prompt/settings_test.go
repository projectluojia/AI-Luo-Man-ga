package prompt

import "testing"

func TestNormalizeSettingsKeepsKnownValuesAndFallsBackUnknown(t *testing.T) {
	settings := NormalizeSettings(Settings{
		UserID:     "user-1",
		BasicStyle: "吐槽达人",
		ExtraTraitLevels: map[string]string{
			"emoji":       "减弱",
			"unknown":     "增强",
			"温和体贴":        "增强",
			"considerate": "减弱",
		},
	})
	if settings.BasicStyle != "roast" {
		t.Fatalf("basic style=%s", settings.BasicStyle)
	}
	if settings.ExtraTraitLevels["emoji"] != "reduced" || settings.ExtraTraitLevels["considerate"] != "reduced" {
		t.Fatalf("levels=%#v", settings.ExtraTraitLevels)
	}
	if _, ok := settings.ExtraTraitLevels["unknown"]; ok {
		t.Fatalf("unknown trait kept: %#v", settings.ExtraTraitLevels)
	}
}
