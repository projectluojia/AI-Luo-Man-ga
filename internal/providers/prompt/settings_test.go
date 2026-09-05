package prompt

import (
	"errors"
	"testing"
)

func TestNormalizeSettingsKeepsKnownValues(t *testing.T) {
	settings, err := NormalizeSettings(Settings{
		UserID:     "user-1",
		BasicStyle: "roast",
		ExtraTraitLevels: map[string]string{
			"emoji":       "reduced",
			"considerate": "reduced",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.BasicStyle != "roast" {
		t.Fatalf("basic style=%s", settings.BasicStyle)
	}
	if settings.ExtraTraitLevels["emoji"] != "reduced" || settings.ExtraTraitLevels["considerate"] != "reduced" {
		t.Fatalf("levels=%#v", settings.ExtraTraitLevels)
	}
}

func TestNormalizeSettingsRejectsUnknownValues(t *testing.T) {
	if _, err := NormalizeSettings(Settings{
		BasicStyle: "unknown",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown basic style error=%v", err)
	}
	if _, err := NormalizeSettings(Settings{
		BasicStyle:       "roast",
		ExtraTraitLevels: map[string]string{"emoji": "unknown"},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown trait level error=%v", err)
	}
}

func TestNormalizeSettingsRejectsNormalizedTraitDuplicates(t *testing.T) {
	if _, err := NormalizeSettings(Settings{
		BasicStyle:       "default",
		ExtraTraitLevels: map[string]string{"emoji": "enhanced", " emoji ": "reduced"},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("normalized duplicate trait error=%v", err)
	}
}
