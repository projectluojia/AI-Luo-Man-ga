package echo_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

func TestPublicRunJSONDoesNotExposeChildResultOrPermissionScope(t *testing.T) {
	encoded, err := json.Marshal(echo.RunRecord{
		ID: "child", RunGroupID: "child", AppID: "app", EchoID: "echo",
		ParentRunID: "parent", OriginCallID: "call",
		CapabilityScope: []string{"campus.bus.routes.list"},
		PermissionScope: []string{"sensitive.permission"},
		Result:          echo.Output{ContentType: "text/plain", Data: []byte("sensitive child result")},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"sensitive.permission", "sensitive child result", "permission_scope"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("public Run JSON disclosed %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"parent_run_id":"parent"`) || !strings.Contains(text, `"capability_scope"`) {
		t.Fatalf("public Run tree metadata missing: %s", text)
	}
}
