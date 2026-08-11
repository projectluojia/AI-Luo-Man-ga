package confirmation

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func FuzzDigest(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"a":1}`))
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(strings.Repeat("a", maxArgumentBytes+1)))
	f.Add([]byte(`{"amount":10,"target":"campus-bus"}`))
	f.Fuzz(func(t *testing.T, arguments []byte) {
		digest, err := Digest(arguments)
		if err != nil {
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Digest 返回未知错误类别: %v", err)
			}
			return
		}
		if len(digest) != digestHexLength {
			t.Fatalf("摘要长度=%d, want %d", len(digest), digestHexLength)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			t.Fatalf("摘要不是十六进制: %v", err)
		}
	})
}
