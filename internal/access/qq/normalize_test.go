package qq

import (
	"encoding/json"
	"testing"
)

type inboundExpect struct {
	channel string
	space   string
	user    string
	session string
	text    string
}

func TestNormalizeEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    *inboundExpect
	}{
		{
			name:    "group message",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m1","message":[{"type":"text","data":{"text":"你好"}}]}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好"},
		},
		{
			name:    "private message with numbers",
			payload: `{"post_type":"message","message_type":"private","user_id":333,"message_id":"m2","message":[{"type":"text","data":{"text":"在吗"}}]}`,
			want:    &inboundExpect{channel: "private", space: "private", user: "333", session: "333", text: "在吗"},
		},
		{
			name:    "text segments concatenated",
			payload: `{"post_type":"message","message_type":"private","user_id":1,"message_id":"m3","message":[{"type":"text","data":{"text":"A"}},{"type":"face","data":{"id":1}},{"type":"text","data":{"text":"B"}}]}`,
			want:    &inboundExpect{channel: "private", space: "private", user: "1", session: "1", text: "AB"},
		},
		{
			name:    "raw message fallback strips cq codes",
			payload: `{"post_type":"message","message_type":"private","user_id":1,"message_id":"m4","raw_message":"你好[CQ:face,id=1]"}`,
			want:    &inboundExpect{channel: "private", space: "private", user: "1", session: "1", text: "你好"},
		},
		{
			name:    "notice event skipped",
			payload: `{"post_type":"notice","notice_type":"poke"}`,
			want:    nil,
		},
		{
			name:    "empty text skipped",
			payload: `{"post_type":"message","message_type":"private","user_id":1,"message_id":"m5","message":[{"type":"image","data":{"file":"a.png"}}]}`,
			want:    nil,
		},
		{
			name:    "missing identity skipped",
			payload: `{"post_type":"message","message_type":"private","message_id":"m6","message":[{"type":"text","data":{"text":"hi"}}]}`,
			want:    nil,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(test.payload), &raw); err != nil {
				t.Fatal(err)
			}
			got := normalizeEvent("campus-services", raw)
			if test.want == nil {
				if got != nil {
					t.Fatalf("got %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want normalized message")
			}
			if got.Platform != "qq" || got.PlatformChannel != test.want.channel ||
				got.PlatformSpaceID != test.want.space ||
				got.PlatformUserID != test.want.user || got.PlatformSessionID != test.want.session ||
				got.Text != test.want.text || got.IdempotencyKey != got.PlatformMessageID {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestSanitizeReplyStripsCQInjection(t *testing.T) {
	got := sanitizeReply("好的\n[CQ:send_group_msg,group_id=1,message=evil]\n\n\n结束")
	if got != "好的\n\n结束" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitReplyChunksAtBoundary(t *testing.T) {
	long := ""
	for len([]rune(long)) < maxReplyChunk*2+10 {
		long += "甲乙丙丁"
	}
	chunks := splitReply(long)
	if len(chunks) < 2 {
		t.Fatalf("chunks=%d", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > maxReplyChunk {
			t.Fatalf("chunk exceeds limit: %d", len([]rune(chunk)))
		}
	}
	if short := splitReply("短回复"); len(short) != 1 || short[0] != "短回复" {
		t.Fatalf("short=%#v", short)
	}
}
