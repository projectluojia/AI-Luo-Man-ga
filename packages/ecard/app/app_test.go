package app

import (
	"encoding/json"
	"testing"

	"github.com/projectluojia/ecard/ecard"
	"github.com/projectluojia/guestkit"
)

// memStore 是窄 store 接口的内存实现：个人作用域以文档映射提供（本包没有
// 系统作用域快照集合）。
type memStore struct {
	user map[string]map[string]json.RawMessage
}

func newMemStore() *memStore {
	return &memStore{user: map[string]map[string]json.RawMessage{}}
}

func (m *memStore) Get(scope guestkit.Scope, collection, id string) (json.RawMessage, bool, error) {
	payload, found := m.user[collection][id]
	return payload, found, nil
}

func (m *memStore) List(scope guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	var docs []guestkit.Document
	for id, payload := range m.user[collection] {
		if afterID == "" || id > afterID {
			docs = append(docs, guestkit.Document{ID: id, Payload: payload})
		}
	}
	if len(docs) > limit {
		docs = docs[:limit]
	}
	if docs == nil {
		docs = []guestkit.Document{}
	}
	return guestkit.ListPage{Docs: docs, MetaFound: false}, nil
}

func (m *memStore) Put(collection, id string, doc json.RawMessage) error {
	if m.user[collection] == nil {
		m.user[collection] = map[string]json.RawMessage{}
	}
	m.user[collection][id] = doc
	return nil
}

func (m *memStore) Delete(string, string) (bool, error) { return false, nil }

func dispatch(t *testing.T, store guestkit.Store, capabilityID string, payload any) guestkit.ResultEnvelope {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return guestkit.NewDispatcher(Handlers(store)).Dispatch(capabilityID, body)
}

func decodeResult(t *testing.T, envelope guestkit.ResultEnvelope, target any) {
	t.Helper()
	if !envelope.OK {
		t.Fatalf("unexpected failure envelope: %s %s", envelope.Code, envelope.Message)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatal(err)
	}
}

func TestEntriesListReturnsDemoCatalog(t *testing.T) {
	var result entriesListResult
	decodeResult(t, dispatch(t, newMemStore(), capEntriesList, ecard.EntriesListRequest{}), &result)
	if len(result.Entries) != 2 {
		t.Fatalf("entries=%+v", result.Entries)
	}
	if result.Entries[0].ID != ecard.EntryIDEcard || result.Entries[1].ID != ecard.EntryIDPay {
		t.Fatalf("entries=%+v", result.Entries)
	}
	if result.DataStatus.Authoritative || result.DataStatus.State != "non_authoritative" {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestCredentialLifecyclePutPrepareRevoke(t *testing.T) {
	store := newMemStore()
	dispatcher := guestkit.NewDispatcher(Handlers(store))

	put := dispatcher.Dispatch(capCredentialsPut, marshal(t, ecard.CredentialsPutRequest{
		Kind: ecard.KindDemoHandle, Material: "demo:handle-1", TTLHours: 2,
	}))
	if !put.OK {
		t.Fatalf("credentials.put: %s %s", put.Code, put.Message)
	}
	var created credentialResult
	if err := json.Unmarshal(put.Result, &created); err != nil {
		t.Fatal(err)
	}
	if created.Credential.CredentialID == "" || created.Credential.Status != ecard.StatusActive {
		t.Fatalf("created=%+v", created.Credential)
	}
	if created.Credential.Fingerprint == "demo:handle-1" {
		t.Fatal("fingerprint must not be the material itself")
	}
	credentialID := created.Credential.CredentialID

	// 会话准备：入口与 UA/头要求来自目录。
	prepare := dispatcher.Dispatch(capSessionPrepare, marshal(t, ecard.SessionPrepareRequest{
		EntryID: ecard.EntryIDEcard, CredentialID: credentialID,
	}))
	if !prepare.OK {
		t.Fatalf("session.prepare: %s %s", prepare.Code, prepare.Message)
	}
	var plan sessionPlanResult
	if err := json.Unmarshal(prepare.Result, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Session.EntryURL != "https://demo.invalid/luojia-ecard" || plan.Session.CredentialFingerprint == "" {
		t.Fatalf("session=%+v", plan.Session)
	}
	if len(plan.Session.RequiredHeaders) != 2 {
		t.Fatalf("headers=%+v", plan.Session.RequiredHeaders)
	}

	// 未知凭据按带内 not-found。
	missing := dispatcher.Dispatch(capSessionPrepare, marshal(t, ecard.SessionPrepareRequest{
		EntryID: ecard.EntryIDEcard, CredentialID: "missing",
	}))
	if !missing.OK {
		t.Fatalf("missing credential should be in-band: %+v", missing)
	}
	var notFound guestkit.NotFound
	if err := json.Unmarshal(missing.Result, &notFound); err != nil || notFound.Found {
		t.Fatalf("notFound=%+v err=%v", notFound, err)
	}

	// 撤销后重复撤销带内冲突，会话准备拒绝。
	revoke := dispatcher.Dispatch(capCredentialsRevoke, marshal(t, ecard.CredentialsRevokeRequest{CredentialID: credentialID}))
	if !revoke.OK {
		t.Fatalf("revoke: %s %s", revoke.Code, revoke.Message)
	}
	var revoked credentialResult
	if err := json.Unmarshal(revoke.Result, &revoked); err != nil || revoked.Credential.Status != ecard.StatusRevoked {
		t.Fatalf("revoked=%+v err=%v", revoked.Credential, err)
	}
	again := dispatcher.Dispatch(capCredentialsRevoke, marshal(t, ecard.CredentialsRevokeRequest{CredentialID: credentialID}))
	if !again.OK {
		t.Fatalf("double revoke should be in-band: %+v", again)
	}
	var conflict guestkit.Conflict
	if err := json.Unmarshal(again.Result, &conflict); err != nil || conflict.Conflict == "" {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}
	blocked := dispatcher.Dispatch(capSessionPrepare, marshal(t, ecard.SessionPrepareRequest{
		EntryID: ecard.EntryIDEcard, CredentialID: credentialID,
	}))
	if !blocked.OK {
		t.Fatalf("blocked session should be in-band: %+v", blocked)
	}
	var unavailable guestkit.Conflict
	if err := json.Unmarshal(blocked.Result, &unavailable); err != nil || unavailable.Conflict != "credential_unavailable" {
		t.Fatalf("unavailable=%+v err=%v", unavailable, err)
	}
}

func TestCredentialStatusReflectsLifecycle(t *testing.T) {
	store := newMemStore()
	dispatcher := guestkit.NewDispatcher(Handlers(store))
	put := dispatcher.Dispatch(capCredentialsPut, marshal(t, ecard.CredentialsPutRequest{
		Kind: ecard.KindDemoHandle, Material: "demo:handle-2",
	}))
	var created credentialResult
	if err := json.Unmarshal(put.Result, &created); err != nil {
		t.Fatal(err)
	}
	var status statusResult
	decodeResult(t, dispatch(t, store, capCredentialsStatus, ecard.CredentialsStatusRequest{CredentialID: created.Credential.CredentialID}), &status)
	if status.Status != ecard.StatusActive {
		t.Fatalf("status=%s", status.Status)
	}
	// 未知凭据返回 not_found 状态（不是错误）。
	decodeResult(t, dispatch(t, store, capCredentialsStatus, ecard.CredentialsStatusRequest{CredentialID: "missing"}), &status)
	if status.Status != ecard.StatusNotFound {
		t.Fatalf("status=%s", status.Status)
	}
}

func TestInvalidPayloadsMapToStableCode(t *testing.T) {
	store := newMemStore()
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("ecard.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
	// 真实凭据材料（无 demo: 前缀）按 invalid_argument 拒绝。
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(capCredentialsPut, marshal(t, ecard.CredentialsPutRequest{
		Kind: ecard.KindDemoHandle, Material: "CASTGC-real-secret",
	})); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("real material envelope=%+v", envelope)
	}
	// 未知字段是协议违例。
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(capEntriesList, json.RawMessage(`{"extra":1}`)); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("unknown field envelope=%+v", envelope)
	}
}

func marshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
