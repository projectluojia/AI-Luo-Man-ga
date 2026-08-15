// Package promptcatalog 是提示词配置的共享契约与默认种子。
//
// 本包保存 V2 迁移来的基础系统提示、渠道提示，以及“可选择项的模板正文”
// （基本风格与额外特征）。控制面、App 配置种子和 prompt Service 共享这些
// 默认值，避免同一份正文在多个包内重复维护。
package promptcatalog

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxNameRunes = 64
	MaxTextRunes = 2 << 10
)

var ErrInvalid = errors.New("invalid prompt catalog")

type BasicStyle struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Text string `json:"text"`
}

type ExtraTrait struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Enhanced string `json:"enhanced"`
	Default  string `json:"default"`
	Reduced  string `json:"reduced"`
}

type Catalog struct {
	BasicStyles []BasicStyle `json:"basic_styles"`
	ExtraTraits []ExtraTrait `json:"extra_traits"`
}

// IsZero 判断目录是否为空（旧配置迁移时用默认目录补齐）。
func (c Catalog) IsZero() bool {
	return len(c.BasicStyles) == 0 && len(c.ExtraTraits) == 0
}

// Clone 返回深拷贝，调用方可以安全修改。
func (c Catalog) Clone() Catalog {
	result := Catalog{
		BasicStyles: make([]BasicStyle, len(c.BasicStyles)),
		ExtraTraits: make([]ExtraTrait, len(c.ExtraTraits)),
	}
	copy(result.BasicStyles, c.BasicStyles)
	copy(result.ExtraTraits, c.ExtraTraits)
	return result
}

// Normalize 校验并规范化目录。零目录返回 V2 默认目录，用于旧配置自动补齐。
// 非零目录必须是完整目录：键集合与默认目录完全一致且顺序一致，只允许修改名称与正文。
func Normalize(catalog Catalog) (Catalog, error) {
	if catalog.IsZero() {
		return Default(), nil
	}
	canonical := Default()
	result := Catalog{
		BasicStyles: make([]BasicStyle, 0, len(canonical.BasicStyles)),
		ExtraTraits: make([]ExtraTrait, 0, len(canonical.ExtraTraits)),
	}
	if len(catalog.BasicStyles) != len(canonical.BasicStyles) ||
		len(catalog.ExtraTraits) != len(canonical.ExtraTraits) {
		return Catalog{}, ErrInvalid
	}
	for index, style := range catalog.BasicStyles {
		normalized, err := normalizeStyle(style, canonical.BasicStyles[index])
		if err != nil {
			return Catalog{}, err
		}
		result.BasicStyles = append(result.BasicStyles, normalized)
	}
	for index, trait := range catalog.ExtraTraits {
		normalized, err := normalizeTrait(trait, canonical.ExtraTraits[index])
		if err != nil {
			return Catalog{}, err
		}
		result.ExtraTraits = append(result.ExtraTraits, normalized)
	}
	return result, nil
}

func normalizeStyle(style BasicStyle, canonical BasicStyle) (BasicStyle, error) {
	if style.Key != canonical.Key {
		return BasicStyle{}, fmt.Errorf("%w: unexpected basic style %q", ErrInvalid, style.Key)
	}
	style.Name = strings.TrimSpace(style.Name)
	style.Text = strings.TrimSpace(style.Text)
	if !validText(style.Name, MaxNameRunes) || !validText(style.Text, MaxTextRunes) {
		return BasicStyle{}, ErrInvalid
	}
	return style, nil
}

func normalizeTrait(trait ExtraTrait, canonical ExtraTrait) (ExtraTrait, error) {
	if trait.Key != canonical.Key {
		return ExtraTrait{}, fmt.Errorf("%w: unexpected extra trait %q", ErrInvalid, trait.Key)
	}
	trait.Name = strings.TrimSpace(trait.Name)
	trait.Enhanced = strings.TrimSpace(trait.Enhanced)
	trait.Default = strings.TrimSpace(trait.Default)
	trait.Reduced = strings.TrimSpace(trait.Reduced)
	if !validText(trait.Name, MaxNameRunes) ||
		!validText(trait.Enhanced, MaxTextRunes) ||
		!validText(trait.Default, MaxTextRunes) ||
		!validText(trait.Reduced, MaxTextRunes) {
		return ExtraTrait{}, ErrInvalid
	}
	return trait, nil
}

func validText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) &&
		utf8.RuneCountInString(value) <= maximum && !strings.ContainsRune(value, '\x00')
}
