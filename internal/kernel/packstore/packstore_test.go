package packstore_test

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
)

// TestValidateScopeAcceptsInternalUserIDs 验证个人作用域 UserID 按
// identity.ValidateUserID 的混合大小写内部标识规则校验（而非包格式的
// 全小写稳定标识）：合法内核用户标识必须通过，空值（系统作用域）不受限。
func TestValidateScopeAcceptsInternalUserIDs(t *testing.T) {
	base := packstore.Scope{AppID: "app-a", PackageID: "test", Namespace: "test/pkg"}
	cases := []struct {
		name    string
		userID  string
		wantErr bool
	}{
		{"mixed case", "Alice", false},
		{"dashed id", "user-a", false},
		{"digits only", "10086", false},
		{"empty is system scope", "", false},
		{"too long", string(make([]byte, 129)[:129])[:129], true},
		{"bad rune", "用户-1", true},
		{"leading separator", ".alice", true},
	}
	for _, tc := range cases {
		scope := base.UserScope(tc.userID)
		err := packstore.ValidateScope(scope)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: ValidateScope(%q) = %v, wantErr %v", tc.name, tc.userID, err, tc.wantErr)
		}
	}
	// 系统/包侧字段仍按包格式校验。
	if err := packstore.ValidateScope(packstore.Scope{AppID: "APP", PackageID: "test", Namespace: "test/pkg"}); err == nil {
		t.Fatal("uppercase AppID must be rejected")
	}
	if err := packstore.ValidateScope(packstore.Scope{AppID: "app-a", PackageID: "test", Namespace: "other/pkg"}); err == nil {
		t.Fatal("namespace outside package must be rejected")
	}
}
