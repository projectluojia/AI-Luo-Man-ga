package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidStableIDAcceptsCanonicalAndRejectsEscapes(t *testing.T) {
	valid := []string{"a", "msg-1", "A1._:-x", "session_0", "direct"}
	for _, value := range valid {
		if !ValidStableID(value) {
			t.Errorf("ValidStableID(%q) 应为 true", value)
		}
	}
	invalid := []string{"", "..", ".", "/abs", "a/b", "a\\b", "a b", strings.Repeat("x", 129), "-leading", "_under", "\x00"}
	for _, value := range invalid {
		if ValidStableID(value) {
			t.Errorf("ValidStableID(%q) 应为 false", value)
		}
	}
}

func TestValidBlobIDAcceptsNamespacesAndRejectsTraversal(t *testing.T) {
	valid := []string{"messages/msg-1", "attachments/att-1", "blob"}
	for _, value := range valid {
		if !ValidBlobID(value) {
			t.Errorf("ValidBlobID(%q) 应为 true", value)
		}
	}
	invalid := []string{
		"", "..", ".", "/abs", "abs/", "/", "messages//msg", "a/../b", "a/b/..",
		"a\\b", "a b", strings.Repeat("x", 257), ":", "messages/../../etc/passwd",
	}
	for _, value := range invalid {
		if ValidBlobID(value) {
			t.Errorf("ValidBlobID(%q) 应为 false", value)
		}
	}
}

func TestValidPlatformMessageIDAllowsEmpty(t *testing.T) {
	if !ValidPlatformMessageID("") {
		t.Error("空平台消息标识应允许（不去重）")
	}
	if !ValidPlatformMessageID("platform-1") {
		t.Error("合法平台消息标识应允许")
	}
	for _, value := range []string{"a b", "平台消息", "../x", strings.Repeat("x", 257)} {
		if ValidPlatformMessageID(value) {
			t.Errorf("ValidPlatformMessageID(%q) 应为 false", value)
		}
	}
}

func TestValidateContentRefBoundsModes(t *testing.T) {
	if err := ValidateContentRef(ContentRef{Mode: ContentModeInline, Size: 5}); err != nil {
		t.Errorf("合法内联引用被拒绝：%v", err)
	}
	if err := ValidateContentRef(ContentRef{Mode: ContentModeBlob, BlobID: "messages/msg-1", Size: 100}); err != nil {
		t.Errorf("合法 Blob 引用被拒绝：%v", err)
	}
	for _, ref := range []ContentRef{
		{Mode: ContentModeInline, Size: 0},
		{Mode: ContentModeInline, BlobID: "x", Size: 5},
		{Mode: ContentModeInline, Size: MaxInlineContentBytes + 1},
		{Mode: ContentModeBlob, Size: 100},
		{Mode: ContentModeBlob, BlobID: "../x", Size: 100},
		{Mode: ContentModeBlob, BlobID: "messages/msg", Size: MaxMessageContentBytes + 1},
		{Mode: "other", Size: 5},
	} {
		if err := ValidateContentRef(ref); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("ValidateContentRef(%#v) 应拒绝，得到 %v", ref, err)
		}
	}
}

func TestValidateSessionEnforcesAppBoundaries(t *testing.T) {
	now := time.Now().UTC()
	valid := Session{
		AppID: "app-a", SessionID: "session-1", Type: SessionTypeGroup,
		Members:   []Member{{UserID: "user-1", Role: MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := ValidateSession(valid); err != nil {
		t.Errorf("合法会话被拒绝：%v", err)
	}
	duplicateMember := valid
	duplicateMember.Members = []Member{
		{UserID: "user-1", Role: MemberRoleOwner, JoinedAt: now},
		{UserID: "user-1", Role: MemberRoleMember, JoinedAt: now},
	}
	if err := ValidateSession(duplicateMember); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("重复成员应被拒绝，得到 %v", err)
	}
	badRole := valid
	badRole.Members[0].Role = "root"
	if err := ValidateSession(badRole); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("非法角色应被拒绝，得到 %v", err)
	}
	duplicateBinding := valid
	duplicateBinding.PlatformBindings = []PlatformBinding{
		{Platform: "qq", PlatformID: "10001", BoundAt: now},
		{Platform: "qq", PlatformID: "10001", BoundAt: now},
	}
	if err := ValidateSession(duplicateBinding); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("重复平台绑定应被拒绝，得到 %v", err)
	}
	for name, mutate := range map[string]func(*Session){
		"坏 App": func(s *Session) { s.AppID = "bad app" },
		"坏类型":   func(s *Session) { s.Type = "unknown" },
		"时间倒置":  func(s *Session) { s.UpdatedAt = now.Add(-time.Hour) },
		"零创建时间": func(s *Session) { s.CreatedAt = time.Time{} },
		"绑定平台标识为空": func(s *Session) {
			s.PlatformBindings = []PlatformBinding{{Platform: "qq", PlatformID: "", BoundAt: now}}
		},
	} {
		mutated := valid
		mutate(&mutated)
		if err := ValidateSession(mutated); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("%s 应被拒绝，得到 %v", name, err)
		}
	}
}

func TestValidateMessageAndEditAndQuery(t *testing.T) {
	now := time.Now().UTC()
	message := Message{
		AppID: "app-a", SessionID: "session-1", MessageID: "msg-1", SenderUserID: "user-1",
		Type: MessageTypeText, ContentRef: ContentRef{Mode: ContentModeInline, Size: 5},
		PlatformMessageID: "platform-1", CreatedAt: now,
	}
	if err := ValidateMessage(message); err != nil {
		t.Errorf("合法消息被拒绝：%v", err)
	}
	blobMessage := message
	blobMessage.Type = MessageTypeImage
	blobMessage.ContentRef = ContentRef{Mode: ContentModeBlob, BlobID: "messages/msg-1", Size: 100}
	if err := ValidateMessage(blobMessage); err != nil {
		t.Errorf("合法 Blob 消息被拒绝：%v", err)
	}
	badReply := message
	badReply.ReplyTo = "../x"
	if err := ValidateMessage(badReply); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("非法回复引用应被拒绝，得到 %v", err)
	}
	badType := message
	badType.Type = "voice"
	if err := ValidateMessage(badType); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("非法消息类型应被拒绝，得到 %v", err)
	}

	edit := MessageEdit{
		AppID: "app-a", SessionID: "session-1", MessageID: "msg-1",
		NewContentRef: ContentRef{Mode: ContentModeInline, Size: 8}, EditedAt: now,
	}
	if err := ValidateMessageEdit(edit); err != nil {
		t.Errorf("合法编辑请求被拒绝：%v", err)
	}
	if err := ValidateMessageEdit(MessageEdit{AppID: "app-a", SessionID: "s", MessageID: "m"}); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("零编辑时间应被拒绝，得到 %v", err)
	}

	query := MessageQuery{Limit: 10}
	if err := ValidateMessageQuery(query); err != nil {
		t.Errorf("合法查询被拒绝：%v", err)
	}
	for _, bad := range []MessageQuery{{Limit: 0}, {Limit: MaxHistoryQueryLimit + 1}, {Limit: -1}} {
		if err := ValidateMessageQuery(bad); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("Limit=%d 应被拒绝，得到 %v", bad.Limit, err)
		}
	}
	if err := ValidateMessageQuery(MessageQuery{Limit: 1, SenderUserID: "../x"}); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("非法发送者过滤应被拒绝，得到 %v", err)
	}
}

func TestValidateAttachmentRejectsPathSeparatorsInFilename(t *testing.T) {
	now := time.Now().UTC()
	attachment := Attachment{
		AppID: "app-a", SessionID: "session-1", AttachmentID: "att-1",
		MessageID: "msg-1", UploaderUserID: "user-1",
		Ref:       AttachmentRef{Filename: "报告.pdf", MimeType: "application/pdf", Size: 100, BlobID: "attachments/att-1"},
		CreatedAt: now,
	}
	if err := ValidateAttachment(attachment); err != nil {
		t.Errorf("合法附件被拒绝：%v", err)
	}
	for _, filename := range []string{"", "../secret.txt", "a/b", "a\\b", "..", ".", "a\x00b", strings.Repeat("x", 257)} {
		mutated := attachment
		mutated.Ref.Filename = filename
		if err := ValidateAttachment(mutated); !errors.Is(err, ErrInvalidAttachment) {
			t.Errorf("文件名 %q 应被拒绝，得到 %v", filename, err)
		}
	}
	for _, ref := range []AttachmentRef{
		{Filename: "a.pdf", MimeType: "application/pdf", Size: 0, BlobID: "attachments/x"},
		{Filename: "a.pdf", MimeType: "application/pdf", Size: MaxAttachmentBytes + 1, BlobID: "attachments/x"},
		{Filename: "a.pdf", MimeType: "application/pdf", Size: 10, BlobID: "../x"},
		{Filename: "a.pdf", MimeType: "not a mime", Size: 10, BlobID: "attachments/x"},
	} {
		if err := ValidateAttachmentRef(ref); !errors.Is(err, ErrInvalidAttachment) {
			t.Errorf("ValidateAttachmentRef(%#v) 应被拒绝，得到 %v", ref, err)
		}
	}
}
