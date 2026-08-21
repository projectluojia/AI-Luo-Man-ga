package session

import (
	"errors"
	"testing"
	"time"
)

func FuzzValidBlobID(f *testing.F) {
	for _, seed := range []string{
		"", "attachments/att-1", "a/b/c", "..", "../x", "a:b", `a\b`, "/abs", "messages/msg-1", "a.b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// 任意输入不得 panic；判定必须确定（同一输入两次结果一致）。
		first := ValidBlobID(value)
		second := ValidBlobID(value)
		if first != second {
			t.Fatalf("ValidBlobID(%q) 判定不确定", value)
		}
	})
}

func FuzzValidateMessage(f *testing.F) {
	for _, seed := range [][7]string{
		{"app-a", "session-1", "msg-1", "user-1", "text", "attachments/x", "msg-0"},
		{"", "", "", "", "", "", ""},
		{"App", "s", "m", "u", "type", "a:b", "r"},
		{"app", "session", "message", "user", "event", "messages/m1", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4], seed[5], seed[6])
	}
	f.Fuzz(func(t *testing.T, appID, sessionID, messageID, sender, messageType, blobID, replyTo string) {
		message := Message{
			AppID:        appID,
			SessionID:    sessionID,
			MessageID:    messageID,
			SenderUserID: sender,
			Type:         messageType,
			ContentRef:   ContentRef{Mode: ContentModeBlob, BlobID: blobID, Size: 1},
			ReplyTo:      replyTo,
			CreatedAt:    time.Unix(1, 0).UTC(),
		}
		// 任意输入不得 panic，错误必须是稳定的 ErrInvalidMessage。
		err := ValidateMessage(message)
		if err != nil && !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("ValidateMessage(%#v) 返回未知错误类别: %v", message, err)
		}
	})
}
