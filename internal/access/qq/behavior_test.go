package qq

import (
	"errors"
	"testing"
)

func TestNormalizeBehaviorCanonicalizesAndRejectsAmbiguousRules(t *testing.T) {
	quick, poke, err := NormalizeBehavior([]QuickReply{
		{Trigger: " ping ", Reply: " pong "},
		{Trigger: "帮助", Reply: "请查看说明"},
	}, DefaultPokeReplies())
	if err != nil {
		t.Fatal(err)
	}
	if len(quick) != 2 || quick[0] != (QuickReply{Trigger: "ping", Reply: "pong"}) || len(poke) == 0 {
		t.Fatalf("quick=%#v poke=%#v", quick, poke)
	}
	if _, _, err := NormalizeBehavior([]QuickReply{{Trigger: "ping", Reply: "a"}, {Trigger: " ping ", Reply: "b"}}, []string{}); !errors.Is(err, ErrInvalidBehavior) {
		t.Fatalf("duplicate trigger error=%v", err)
	}
}

func TestNormalizeBehaviorAllowsDisablingPokeText(t *testing.T) {
	_, poke, err := NormalizeBehavior(nil, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if poke == nil || len(poke) != 0 {
		t.Fatalf("poke=%#v", poke)
	}
}
