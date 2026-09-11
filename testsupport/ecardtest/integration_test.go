//go:build integration

// 珞珈 E 卡 hosted 包端到端：真实 ailuo.toml 清单 + 真实 guest 源码现场编译 +
// packstore 个人凭据文档（用户作用域）。覆盖入口目录（显式非权威）、凭据
// 生命周期（确认门槛 + 指纹脱敏 + 撤销冲突 + 会话拒绝）与真实材料拒绝，
// 全部经真实 Dispatcher。
package ecardtest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// packageID 返回 ecard 包的真实包 ID（来自 ailuo.toml 清单）。
func packageID(t *testing.T) string {
	return hostedtest.ManifestOf(t, "ecard").ID
}

func newEcardDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	hostedtest.RegisterHosted(t, reg, store, "ecard")
	return hostedtest.NewDispatcher(t, reg, packageID(t), hostedtest.CapabilityIDs(t, "ecard"))
}

func invoke(t *testing.T, d *runtime.Dispatcher, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, packageID(t), capabilityID, payload, idempotencyKey, confirmationID)
}

// TestHostedEcardEntriesList 经真实 wasm guest 列出双入口目录：显式
// non_authoritative（演示形态，不是治理失败）。
func TestHostedEcardEntriesList(t *testing.T) {
	d := newEcardDispatcher(t)
	ok, result, errText := invoke(t, d, "ecard.entries.list", `{}`, "", "")
	if !ok {
		t.Fatalf("entries.list failed: %s", errText)
	}
	var decoded struct {
		DataStatus struct {
			State         string `json:"state"`
			Authoritative bool   `json:"authoritative"`
		} `json:"data_status"`
		Entries []struct {
			ID                    string `json:"id"`
			EntryURL              string `json:"entry_url"`
			RequiresDelegatedAuth bool   `json:"requires_delegated_auth"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("entries.list result %s: %v", result, err)
	}
	if decoded.DataStatus.Authoritative || decoded.DataStatus.State != "non_authoritative" {
		t.Fatalf("data_status = %+v", decoded.DataStatus)
	}
	if len(decoded.Entries) != 2 || decoded.Entries[0].ID != "luojia_ecard" || decoded.Entries[1].ID != "ecard_paycode" {
		t.Fatalf("entries = %#v", decoded.Entries)
	}
	for _, entry := range decoded.Entries {
		if !entry.RequiresDelegatedAuth || entry.EntryURL == "" {
			t.Fatalf("entry = %#v", entry)
		}
	}
}

// TestHostedEcardCredentialLifecycle 经真实 wasm guest 走通凭据生命周期：
// 存入演示材料（确认门槛 + 指纹脱敏）→ 会话准备 → 撤销（确认门槛）→
// 重复撤销带内冲突 → 已撤销凭据会话拒绝。
func TestHostedEcardCredentialLifecycle(t *testing.T) {
	d := newEcardDispatcher(t)

	// 存入是确认门槛能力：无确认被 dispatcher 前置拒绝。
	ok, _, errText := invoke(t, d, "ecard.credentials.put",
		`{"kind":"demo_handle","material":"demo:handle-1"}`, "idem-put", "")
	if ok {
		t.Fatal("credentials.put without confirmation should fail")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("put error = %q, want confirmation required", errText)
	}

	ok, result, errText := invoke(t, d, "ecard.credentials.put",
		`{"kind":"demo_handle","material":"demo:handle-1","ttl_hours":2}`, "idem-put", "confirm-put")
	if !ok {
		t.Fatalf("credentials.put failed: %s", errText)
	}
	var created struct {
		Credential struct {
			CredentialID string `json:"credential_id"`
			Status       string `json:"status"`
			Fingerprint  string `json:"fingerprint"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("put result %s: %v", result, err)
	}
	if created.Credential.CredentialID == "" || created.Credential.Status != "active" {
		t.Fatalf("created = %#v", created.Credential)
	}
	if created.Credential.Fingerprint == "" || created.Credential.Fingerprint == "demo:handle-1" {
		t.Fatalf("fingerprint must be derived, not the material: %q", created.Credential.Fingerprint)
	}
	credentialID := created.Credential.CredentialID

	// 会话准备：入口 URL 与 UA/头要求来自目录，含凭据指纹不含秘密。
	ok, result, errText = invoke(t, d, "ecard.session.prepare",
		`{"entry_id":"luojia_ecard","credential_id":"`+credentialID+`"}`, "", "")
	if !ok {
		t.Fatalf("session.prepare failed: %s", errText)
	}
	var plan struct {
		Session struct {
			EntryURL              string `json:"entry_url"`
			CredentialFingerprint string `json:"credential_fingerprint"`
			RequiredHeaders       []struct {
				Name string `json:"name"`
			} `json:"required_headers"`
		} `json:"session"`
	}
	if err := json.Unmarshal(result, &plan); err != nil {
		t.Fatalf("session result %s: %v", result, err)
	}
	if plan.Session.EntryURL != "https://demo.invalid/luojia-ecard" || plan.Session.CredentialFingerprint == "" {
		t.Fatalf("session = %#v", plan.Session)
	}
	if len(plan.Session.RequiredHeaders) != 2 {
		t.Fatalf("headers = %#v", plan.Session.RequiredHeaders)
	}

	// 状态查询：active。
	ok, result, errText = invoke(t, d, "ecard.credentials.status",
		`{"credential_id":"`+credentialID+`"}`, "", "")
	if !ok {
		t.Fatalf("credentials.status failed: %s", errText)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(result, &status); err != nil || status.Status != "active" {
		t.Fatalf("status = %#v err=%v result=%s", status, err, result)
	}

	// 撤销是确认门槛能力。
	ok, result, errText = invoke(t, d, "ecard.credentials.revoke",
		`{"credential_id":"`+credentialID+`"}`, "idem-revoke", "confirm-revoke")
	if !ok {
		t.Fatalf("credentials.revoke failed: %s", errText)
	}
	var revoked struct {
		Credential struct {
			Status string `json:"status"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(result, &revoked); err != nil || revoked.Credential.Status != "revoked" {
		t.Fatalf("revoked = %#v err=%v", revoked.Credential, err)
	}

	// 重复撤销带内冲突。
	ok, result, errText = invoke(t, d, "ecard.credentials.revoke",
		`{"credential_id":"`+credentialID+`"}`, "idem-revoke-2", "confirm-revoke")
	if !ok {
		t.Fatalf("double revoke should be in-band: %s", errText)
	}
	var conflict struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &conflict); err != nil || conflict.Conflict == "" {
		t.Fatalf("conflict = %#v err=%v result=%s", conflict, err, result)
	}

	// 已撤销凭据的会话准备按带内冲突拒绝。
	ok, result, errText = invoke(t, d, "ecard.session.prepare",
		`{"entry_id":"luojia_ecard","credential_id":"`+credentialID+`"}`, "", "")
	if !ok {
		t.Fatalf("blocked session should be in-band: %s", errText)
	}
	var unavailable struct {
		Conflict string `json:"conflict"`
	}
	if err := json.Unmarshal(result, &unavailable); err != nil || unavailable.Conflict != "credential_unavailable" {
		t.Fatalf("unavailable = %#v err=%v result=%s", unavailable, err, result)
	}
}

// TestHostedEcardRejectsRealMaterial 真实凭据材料（无 demo: 前缀 / 含 CAS
// 特征）按稳定 invalid_argument 拒绝（fail-closed；消息不外泄细节）。
func TestHostedEcardRejectsRealMaterial(t *testing.T) {
	d := newEcardDispatcher(t)
	for name, payload := range map[string]string{
		"no_prefix":    `{"kind":"demo_handle","material":"CASTGC-real-secret"}`,
		"castgc_token": `{"kind":"demo_handle","material":"demo:x-castgc-leak"}`,
		"cas_host":     `{"kind":"demo_handle","material":"demo:https://cas.whu.edu.cn/auth"}`,
		"st_ticket":    `{"kind":"demo_handle","material":"demo:ST-12345-abc"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, errText := invoke(t, d, "ecard.credentials.put", payload, "idem-"+name, "confirm-put")
			if errText == "" {
				t.Fatal("real material should be rejected")
			}
			// 内核只透出拒绝事实（"hosted package rejected the call"），稳定
			// 错误码在 InvocationError.Code 内，消息不外泄细节。
			if !strings.Contains(errText, "hosted package rejected the call") {
				t.Fatalf("error = %q, want hosted rejection", errText)
			}
		})
	}
	// 状态查询不泄漏：未知凭据返回 not_found 状态（不是错误）。
	ok, result, errText := invoke(t, d, "ecard.credentials.status", `{"credential_id":"missing"}`, "", "")
	if !ok {
		t.Fatalf("missing status should be in-band: %s", errText)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(result, &status); err != nil || status.Status != "not_found" {
		t.Fatalf("status = %#v err=%v", status, err)
	}
}
