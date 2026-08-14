package qq

import (
	"encoding/json"
	"testing"
)

const testBotQQID = "2647414417"

type inboundExpect struct {
	channel   string
	space     string
	user      string
	session   string
	text      string
	mentioned bool
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
			name:    "group at mention flagged",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m7","message":[{"type":"at","data":{"qq":"2647414417"}},{"type":"text","data":{"text":"有哪些线路"}}]}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "有哪些线路", mentioned: true},
		},
		{
			name:    "group at another user not flagged",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m9","message":[{"type":"at","data":{"qq":"123456"}},{"type":"text","data":{"text":"你好"}}]}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好"},
		},
		{
			name:    "group at all not flagged",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m10","message":[{"type":"at","data":{"qq":"all"}},{"type":"text","data":{"text":"你好"}}]}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好"},
		},
		{
			name:    "multiple mentions include bot",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m11","message":[{"type":"at","data":{"qq":"123456"}},{"type":"at","data":{"qq":"2647414417"}},{"type":"text","data":{"text":"你好"}}]}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好", mentioned: true},
		},
		{
			name:    "raw cq mentions bot",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m12","raw_message":"[CQ:at,name=AI珞,qq=2647414417] 你好"}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好", mentioned: true},
		},
		{
			name:    "raw cq mentions another user",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m13","raw_message":"[CQ:at,qq=123456] 你好"}`,
			want:    &inboundExpect{channel: "group", space: "111", user: "222", session: "111", text: "你好"},
		},
		{
			name:    "bot self message skipped",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":2647414417,"message_id":"m14","message":[{"type":"text","data":{"text":"机器人消息"}}]}`,
			want:    nil,
		},
		{
			name:    "mismatched self id skipped",
			payload: `{"post_type":"message","self_id":999999,"message_type":"group","group_id":111,"user_id":222,"message_id":"m15","message":[{"type":"at","data":{"qq":"2647414417"}},{"type":"text","data":{"text":"你好"}}]}`,
			want:    nil,
		},
		{
			name:    "group at only no text skipped",
			payload: `{"post_type":"message","message_type":"group","group_id":111,"user_id":222,"message_id":"m8","message":[{"type":"at","data":{"qq":"2647414417"}}]}`,
			want:    nil,
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
			got, mentioned := normalizeEvent("campus-services", testBotQQID, raw)
			if test.want == nil {
				if got != nil || mentioned {
					t.Fatalf("got %#v mentioned=%t, want nil and false", got, mentioned)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want normalized message")
			}
			if got.Platform != "qq" || got.PlatformChannel != test.want.channel ||
				got.PlatformSpaceID != test.want.space ||
				got.PlatformUserID != test.want.user || got.PlatformSessionID != test.want.session ||
				got.Text != test.want.text || got.IdempotencyKey != got.PlatformMessageID ||
				mentioned != test.want.mentioned {
				t.Fatalf("got %#v mentioned=%t", got, mentioned)
			}
		})
	}
}

func TestNormalizeQQID(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{value: testBotQQID, valid: true},
		{value: "", valid: false},
		{value: " 2647414417", valid: false},
		{value: "2647414417 ", valid: false},
		{value: "02647414417", valid: false},
		{value: "-1", valid: false},
		{value: "all", valid: false},
		{value: "0", valid: false},
	}
	for _, test := range tests {
		normalized, valid := normalizeQQID(test.value)
		if valid != test.valid {
			t.Errorf("normalizeQQID(%q) valid=%t, want %t", test.value, valid, test.valid)
		}
		if valid && normalized != test.value {
			t.Errorf("normalizeQQID(%q)=%q", test.value, normalized)
		}
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
