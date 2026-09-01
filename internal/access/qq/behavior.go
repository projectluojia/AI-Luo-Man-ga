package qq

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxQuickReplies       = 64
	MaxPokeReplies        = 64
	MaxQuickTriggerRunes  = 128
	MaxBehaviorReplyRunes = 4000
)

var ErrInvalidBehavior = errors.New("invalid QQ access behavior configuration")

// QuickReply 是 QQ Access 的精确快速回复规则；它不进入 Executor、Service 或 Tool。
type QuickReply struct {
	Trigger string `json:"trigger"`
	Reply   string `json:"reply"`
}

var defaultPokeReplies = []string{
	"🌸", "(轻轻戳回去)", "(探头)", "😊", "(轻轻歪头)嗯？", "(´▽｀)ノ♪",
	"诶？", "(轻轻挥手)", "(戳回去)", "(◕ᴗ◕✿)", "(´▽｀)ノ", "(✧ω✧)",
}

// DefaultPokeReplies 返回默认戳一戳文案副本，调用方可以安全修改。
func DefaultPokeReplies() []string {
	return append([]string(nil), defaultPokeReplies...)
}

// NormalizeBehavior 规范化平台快速行为配置。空切片表示禁用戳一戳文本回复。
func NormalizeBehavior(quickReplies []QuickReply, pokeReplies []string) ([]QuickReply, []string, error) {
	if len(quickReplies) > MaxQuickReplies || len(pokeReplies) > MaxPokeReplies {
		return nil, nil, ErrInvalidBehavior
	}
	quick := make([]QuickReply, 0, len(quickReplies))
	triggers := make(map[string]struct{}, len(quickReplies))
	for _, rule := range quickReplies {
		rule.Trigger = strings.TrimSpace(rule.Trigger)
		rule.Reply = strings.TrimSpace(rule.Reply)
		if !validBehaviorText(rule.Trigger, MaxQuickTriggerRunes) ||
			!validBehaviorText(rule.Reply, MaxBehaviorReplyRunes) {
			return nil, nil, ErrInvalidBehavior
		}
		if _, duplicate := triggers[rule.Trigger]; duplicate {
			return nil, nil, ErrInvalidBehavior
		}
		triggers[rule.Trigger] = struct{}{}
		quick = append(quick, rule)
	}
	sort.Slice(quick, func(left, right int) bool { return quick[left].Trigger < quick[right].Trigger })
	poke := make([]string, 0, len(pokeReplies))
	for _, reply := range pokeReplies {
		reply = strings.TrimSpace(reply)
		if !validBehaviorText(reply, MaxBehaviorReplyRunes) {
			return nil, nil, ErrInvalidBehavior
		}
		poke = append(poke, reply)
	}
	return quick, poke, nil
}

func validBehaviorText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum
}
