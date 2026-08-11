package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func FuzzValidateUserID(f *testing.F) {
	for _, seed := range []string{"", "user-1", "a", "UPPER", "a b", "用户", strings.Repeat("a", 200), "a.b:c_d"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// 任意输入不得 panic，错误必须是稳定的 ErrInvalid。
		err := ValidateUserID(value)
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateUserID(%q) 返回未知错误类别: %v", value, err)
		}
	})
}

func FuzzValidatePlatformUserID(f *testing.F) {
	for _, seed := range []string{"", "qq-10001", strings.Repeat("x", 300), "包含\u0000控制字符", "ok", "空格 ok"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		err := ValidatePlatformUserID(value)
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidatePlatformUserID(%q) 返回未知错误类别: %v", value, err)
		}
	})
}

func FuzzNormalizeExternalIdentity(f *testing.F) {
	for _, seed := range [][5]string{
		{"app-a", "qq", "space-1", "user-1", "user-1"},
		{"", "", "", "", ""},
		{"App-A", "QQ", "space 1", "u", "user"},
		{"a", "b", "c", "d", "e"},
		{"app", "qq", "space", "  带空格的平台标识  ", "user-1"},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
	}
	f.Fuzz(func(t *testing.T, appID, platform, spaceID, platformUserID, userID string) {
		binding := ExternalIdentity{
			AppID: appID, Platform: platform, PlatformSpaceID: spaceID,
			PlatformUserID: platformUserID, UserID: userID,
			BoundAt: time.Unix(0, 0).UTC(),
		}
		normalized, err := NormalizeExternalIdentity(binding)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NormalizeExternalIdentity(%#v) 返回未知错误类别: %v", binding, err)
			}
			return
		}
		// 不变量：规范化幂等；合法输入的规范化结果必须通过整体校验。
		again, err := NormalizeExternalIdentity(normalized)
		if err != nil || again != normalized {
			t.Fatalf("规范化不幂等: %#v -> %#v err=%v", normalized, again, err)
		}
		if err := ValidateExternalIdentity(normalized); err != nil {
			t.Fatalf("规范化结果未通过校验: %v", err)
		}
	})
}
