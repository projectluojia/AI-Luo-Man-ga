// Package app 是珞珈 E 卡双入口包的协议层：5 个 Capability 的载荷分发。
// 入口目录是包内演示常量（显式 non_authoritative 标注），凭据是个人作用域
// 文档（宿主从治理上下文注入 UserID）。
//
// 错误面与 classroom/library/sports 同型：ok:false 只保留 invalid_argument
// 与数据治理闭式错误码；not_found/conflict 等业务失败在 ok:true 的结果载荷
// 内表达。凭据材料本身不落库（只存指纹）——真实凭据留给阶段 2c 的签名
// 学校认证包。
package app

import (
	"encoding/json"
	"time"

	"github.com/projectluojia/ecard/ecard"
	"github.com/projectluojia/guestkit"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	capEntriesList       = "ecard.entries.list"
	capSessionPrepare    = "ecard.session.prepare"
	capCredentialsPut    = "ecard.credentials.put"
	capCredentialsRevoke = "ecard.credentials.revoke"
	capCredentialsStatus = "ecard.credentials.status"
)

var errInvalidArgument = guestkit.ErrInvalidArgument

// NewDispatcher 构造分发器：以 guestkit 共享 Dispatcher 分发（唯一实现，
// 包内只声明能力分发表）。store 为 guestkit.StoreClient（wasip1 内）或测试替身。
func NewDispatcher(client guestkit.Store) *guestkit.Dispatcher {
	return guestkit.NewDispatcher(Handlers(client))
}

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		capEntriesList:       func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listEntries) },
		capSessionPrepare:    func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.prepareSession) },
		capCredentialsPut:    func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.putCredential) },
		capCredentialsRevoke: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.revokeCredential) },
		capCredentialsStatus: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.credentialStatus) },
	}
}

// ---- 入口目录（演示常量） ----

type entriesListResult struct {
	DataStatus ecard.DataStatus `json:"data_status"`
	Entries    []ecard.Entry    `json:"entries"`
}

func (d *dispatcher) listEntries(_ ecard.EntriesListRequest) (any, error) {
	entries, status := ecard.DemoEntries(time.Now())
	return entriesListResult{DataStatus: status, Entries: entries}, nil
}

// ---- 凭据与会话（个人作用域） ----

type sessionPlanResult struct {
	Session ecard.SessionPlan `json:"session"`
}

type credentialResult struct {
	Credential ecard.Credential `json:"credential"`
}

type statusResult struct {
	Status string `json:"status"`
}

func (d *dispatcher) prepareSession(request ecard.SessionPrepareRequest) (any, error) {
	credential, err := d.myCredential(request.CredentialID)
	if err != nil {
		return nil, err
	}
	if credential == nil {
		return guestkit.NotFound{}, nil
	}
	now := time.Now().UTC()
	if credential.EffectiveStatus(now) != ecard.StatusActive {
		// 已撤销/过期的凭据不能建立会话（带内冲突，不泄露原因细节）。
		return guestkit.Conflict{Conflict: "credential_unavailable"}, nil
	}
	// 目录与状态只取一次：同一请求内不重复构造 DemoEntries。
	entries, status := ecard.DemoEntries(now)
	var entry ecard.Entry
	known := false
	for _, candidate := range entries {
		if candidate.ID == request.EntryID {
			entry, known = candidate, true
			break
		}
	}
	if !known {
		return nil, errInvalidArgument
	}
	// 会话有效期随凭据到期，不超过目录有效窗口。
	expiresAt := credential.ExpiresAt
	if validUntil, err := time.Parse(time.RFC3339, status.ValidUntil); err == nil {
		if credentialExpiresAt, err := time.Parse(time.RFC3339, credential.ExpiresAt); err == nil && validUntil.Before(credentialExpiresAt) {
			expiresAt = status.ValidUntil
		}
	}
	plan := ecard.SessionPlan{
		EntryID: request.EntryID, EntryURL: entry.EntryURL,
		RequiredUserAgent:     entry.RequiredUserAgent,
		RequiredHeaders:       entry.RequiredHeaders,
		CredentialFingerprint: credential.Fingerprint,
		ExpiresAt:             expiresAt,
	}
	return sessionPlanResult{Session: plan}, nil
}

func (d *dispatcher) putCredential(request ecard.CredentialsPutRequest) (any, error) {
	// NormalizeAndValidate 已强制 demo: 前缀并拒绝真实凭据特征的材料。
	now := time.Now().UTC()
	item := ecard.Credential{
		CredentialID: guestkit.NewDocID("ec"), Kind: request.Kind,
		Status: ecard.StatusActive, Fingerprint: ecard.FingerprintOf(request.Material),
		CreatedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Duration(request.TTLHours) * time.Hour).Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(ecard.CredentialsCollection, item.CredentialID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return credentialResult{Credential: item}, nil
}

func (d *dispatcher) revokeCredential(request ecard.CredentialsRevokeRequest) (any, error) {
	item, err := d.myCredential(request.CredentialID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return guestkit.NotFound{}, nil
	}
	if item.Status == ecard.StatusRevoked {
		return guestkit.Conflict{Conflict: request.CredentialID}, nil
	}
	now := time.Now().UTC()
	item.Status = ecard.StatusRevoked
	item.RevokedAt = now.Format(time.RFC3339Nano)
	if err := d.saveCredential(*item); err != nil {
		return nil, err
	}
	return credentialResult{Credential: *item}, nil
}

func (d *dispatcher) credentialStatus(request ecard.CredentialsStatusRequest) (any, error) {
	item, err := d.myCredential(request.CredentialID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return statusResult{Status: ecard.StatusNotFound}, nil
	}
	return statusResult{Status: item.EffectiveStatus(time.Now().UTC())}, nil
}

func (d *dispatcher) myCredential(credentialID string) (*ecard.Credential, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, ecard.CredentialsCollection, credentialID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return nil, nil
	}
	var item ecard.Credential
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	return &item, nil
}

func (d *dispatcher) saveCredential(item ecard.Credential) error {
	payload, err := json.Marshal(item)
	if err != nil {
		return guestkit.ErrInternal
	}
	if err := d.store.Put(ecard.CredentialsCollection, item.CredentialID, payload); err != nil {
		return guestkit.ErrInternal
	}
	return nil
}
