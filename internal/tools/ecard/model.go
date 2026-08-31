// Package ecard 提供珞珈 E 卡 / 付款码双入口 WebView 会话的原子 Tool 模型。
// 内核只治理入口描述、智能校园 UA/Header 名称与委托凭据密文；不抓取智慧珞珈
// 接口，不生成付款码或二维码载荷。用户身份只来自 RequestContext.UserID。
package ecard

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	ServiceID = "ecard"

	EntriesListToolID       = "ecard.entries.list"
	SessionPrepareToolID    = "ecard.session.prepare"
	CredentialsPutToolID    = "ecard.credentials.put"
	CredentialsRevokeToolID = "ecard.credentials.revoke"
	CredentialsStatusToolID = "ecard.credentials.status"

	EntryIDLuoJiaECard = "luojia_ecard"
	EntryIDPayCode     = "ecard_paycode"

	EntryKindLuoJiaECard = "luojia_ecard"
	EntryKindPayCode     = "paycode"

	KindCASCookie  = "cas_cookie"
	KindDemoHandle = "demo_handle"

	UserAgentPurposeSmartCampus = "smart_campus"
	HeaderRequestedWith         = "X-Requested-With"
	HeaderReferer               = "Referer"

	// PublicCASEntryURL 是公开文档中的武大统一身份认证入口主机，不是隐藏接口。
	PublicCASEntryURL = "https://cas.whu.edu.cn/"
	DemoECardEntryURL = "https://demo.invalid/luojia-ecard"
	DemoPayCodeURL    = "https://demo.invalid/ecard-paycode"

	DemoSource                   = "demo-fixture-not-zhihui-luojia"
	ProductionCatalogSource      = "kernel-catalog-unauthorized-integration"
	CatalogRevision              = "ecard-catalog-v1"
	DataStateNonAuthoritative    = "non_authoritative"
	GovernedSmartCampusUserAgent = "SmartCampus/WHU (purpose=smart_campus; governed-by=ailuo)"
	DemoUserAgent                = "AILuo-Demo/1.0 (purpose=smart_campus; data_status=non_authoritative)"
	DemoRequestedWith            = "demo.ailuo.ecard"
	ProductionRequestedWith      = "XMLHttpRequest"

	CookieNameCASTGC     = "CASTGC"
	CookieNameJSESSIONID = "JSESSIONID"

	MaxMaterialBytes = 4096
	MaxMaterialRunes = 4096
	MaxCredentialTTL = 7 * 24 * time.Hour
	GCMNonceSize     = 12
	MaxCiphertext    = 8192
	MinCiphertext    = 16
	AES256KeySize    = 32

	DemoMaterialPrefix = "demo:"
)

var (
	ErrInvalid            = errors.New("invalid ecard input")
	ErrUserRequired       = errors.New("ecard user is required")
	ErrNotFound           = errors.New("ecard credential is not found")
	ErrKeyUnavailable     = errors.New("ecard credential key is unavailable")
	ErrKeyInvalid         = errors.New("ecard credential key is invalid")
	ErrDemoMaterial       = errors.New("demo ecard material is not allowed")
	ErrProductionMaterial = errors.New("production ecard material is not allowed in demo mode")
	ErrExpired            = errors.New("ecard credential has expired")
)

const (
	EntriesListInputSchemaJSON       = `{"type":"object","additionalProperties":false}`
	SessionPrepareInputSchemaJSON    = `{"type":"object","properties":{"entry_id":{"type":"string","enum":["luojia_ecard","ecard_paycode"]},"credential_handle":{"type":"string","minLength":1,"maxLength":128,"pattern":"^[a-z][a-z0-9_]*$"}},"required":["entry_id","credential_handle"],"additionalProperties":false}`
	CredentialsPutInputSchemaJSON    = `{"type":"object","properties":{"kind":{"type":"string","enum":["cas_cookie","demo_handle"]},"credential_material":{"type":"string","minLength":1,"maxLength":4096},"expires_at":{"type":"string","format":"date-time","maxLength":64}},"required":["kind","credential_material","expires_at"],"additionalProperties":false}`
	CredentialsRevokeInputSchemaJSON = `{"type":"object","properties":{"kind":{"type":"string","enum":["cas_cookie","demo_handle"]},"credential_handle":{"type":"string","minLength":1,"maxLength":128,"pattern":"^[a-z][a-z0-9_]*$"}},"additionalProperties":false}`
	CredentialsStatusInputSchemaJSON = `{"type":"object","properties":{"kind":{"type":"string","enum":["cas_cookie","demo_handle"]}},"additionalProperties":false}`
)

// CredentialRecord 是落库的委托凭据密文行；不含明文。
type CredentialRecord struct {
	AppID      string
	UserID     string
	Kind       string
	Nonce      []byte
	Ciphertext []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

// CredentialMeta 是不含密文的凭据状态。
type CredentialMeta struct {
	Kind      string
	Handle    string
	CreatedAt time.Time
	ExpiresAt time.Time
	Present   bool
	Revoked   bool
}

// Store 是 Go 管理的 E 卡凭据存储端口。实现必须按 app_id+user_id 隔离，且只持久化密文。
type Store interface {
	PutECardCredential(ctx context.Context, record CredentialRecord) (CredentialRecord, error)
	GetActiveECardCredential(ctx context.Context, appID, userID, kind string) (CredentialRecord, error)
	RevokeECardCredential(ctx context.Context, appID, userID, kind string, at time.Time) error
	GetECardCredentialMeta(ctx context.Context, appID, userID, kind string) (CredentialMeta, error)
}

// DataStatus 是入口/会话描述的治理状态；演示数据必须显式非权威。
type DataStatus struct {
	State          string    `json:"state"`
	Source         string    `json:"source"`
	Authoritative  bool      `json:"authoritative"`
	Complete       bool      `json:"complete"`
	SourceRevision string    `json:"source_revision"`
	FetchedAt      time.Time `json:"fetched_at"`
	ValidUntil     time.Time `json:"valid_until"`
}

// Entry 是双入口目录中的一项，不含 Cookie 值或付款码载荷。
type Entry struct {
	ID                    string     `json:"id"`
	Title                 string     `json:"title"`
	EntryKind             string     `json:"entry_kind"`
	RequiresDelegatedAuth bool       `json:"requires_delegated_auth"`
	RequiredUserAgent     string     `json:"required_user_agent_purpose"`
	RequiredHeaderNames   []string   `json:"required_header_names"`
	DataStatus            DataStatus `json:"data_status"`
}

// SessionPlan 是客户端 WebView 应应用的会话描述：不含 Cookie 值。
type SessionPlan struct {
	EntryID     string            `json:"entry_id"`
	EntryURL    string            `json:"entry_url"`
	UserAgent   string            `json:"user_agent"`
	Headers     map[string]string `json:"headers"`
	CookieNames []string          `json:"cookie_names"`
	ExpiresAt   time.Time         `json:"expires_at"`
	DataStatus  DataStatus        `json:"data_status"`
}

// CredentialPutResult 是写入委托凭据后的句柄回执，不含明文。
type CredentialPutResult struct {
	CredentialHandle string    `json:"credential_handle"`
	Kind             string    `json:"kind"`
	HasCredential    bool      `json:"has_credential"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// CredentialStatusResult 是凭据存在性查询结果，不含秘密。
type CredentialStatusResult struct {
	HasCredential    bool       `json:"has_credential"`
	Kind             string     `json:"kind,omitempty"`
	CredentialHandle string     `json:"credential_handle,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// Config 装配 E 卡 Tool：密钥在测试中注入，生产缺失时 fail-closed。
type Config struct {
	Store      Store
	Key        []byte
	DemoMode   bool
	Production bool
	Now        func() time.Time
}

func (c Config) clock() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

func (c Config) aesKey() ([]byte, error) {
	if len(c.Key) == 0 {
		return nil, ErrKeyUnavailable
	}
	if len(c.Key) != AES256KeySize {
		return nil, ErrKeyInvalid
	}
	key := make([]byte, AES256KeySize)
	copy(key, c.Key)
	return key, nil
}

func requireUser(request contracts.RequestContext) error {
	if request.UserID == "" {
		return errors.Join(registry.ErrPermissionDenied, ErrUserRequired)
	}
	if err := identity.ValidateUserID(request.UserID); err != nil {
		return errors.Join(registry.ErrPermissionDenied, ErrUserRequired)
	}
	if err := identity.ValidateAppID(request.AppID); err != nil {
		return errors.Join(registry.ErrPermissionDenied, ErrInvalid)
	}
	return nil
}

func validKind(kind string) bool {
	return kind == KindCASCookie || kind == KindDemoHandle
}

func handleForKind(kind string) string {
	return kind
}

func kindFromHandle(handle string) (string, error) {
	handle = strings.TrimSpace(handle)
	if !validKind(handle) {
		return "", ErrInvalid
	}
	return handle, nil
}

func looksLikeRealDelegatedMaterial(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "castgc") || strings.Contains(lower, "cas.whu.edu.cn") {
		return true
	}
	if strings.Contains(value, "TGT-") || strings.Contains(value, "ST-") {
		return true
	}
	return false
}

func validatePutMaterial(kind, material string, demoMode, production bool) error {
	if !validKind(kind) {
		return ErrInvalid
	}
	if material == "" || len(material) > MaxMaterialBytes || utf8.RuneCountInString(material) > MaxMaterialRunes {
		return ErrInvalid
	}
	if demoMode {
		if kind != KindDemoHandle || !strings.HasPrefix(material, DemoMaterialPrefix) || looksLikeRealDelegatedMaterial(material) {
			return errors.Join(contracts.ErrDataUntrusted, ErrProductionMaterial)
		}
		return nil
	}
	if kind == KindDemoHandle {
		return errors.Join(contracts.ErrDataUntrusted, ErrDemoMaterial)
	}
	if production && kind != KindCASCookie {
		return errors.Join(contracts.ErrDataUntrusted, ErrDemoMaterial)
	}
	if strings.HasPrefix(material, DemoMaterialPrefix) {
		return errors.Join(contracts.ErrDataUntrusted, ErrDemoMaterial)
	}
	return nil
}

func catalogStatus(now time.Time, demoMode bool) DataStatus {
	source := ProductionCatalogSource
	if demoMode {
		source = DemoSource
	}
	return DataStatus{
		State:          DataStateNonAuthoritative,
		Source:         source,
		Authoritative:  false,
		Complete:       true,
		SourceRevision: CatalogRevision,
		FetchedAt:      now.UTC(),
		ValidUntil:     now.UTC().Add(24 * time.Hour),
	}
}

func dualEntries(status DataStatus) []Entry {
	headers := []string{HeaderRequestedWith, HeaderReferer}
	return []Entry{
		{
			ID:                    EntryIDLuoJiaECard,
			Title:                 "珞珈E卡",
			EntryKind:             EntryKindLuoJiaECard,
			RequiresDelegatedAuth: true,
			RequiredUserAgent:     UserAgentPurposeSmartCampus,
			RequiredHeaderNames:   append([]string(nil), headers...),
			DataStatus:            status,
		},
		{
			ID:                    EntryIDPayCode,
			Title:                 "付款码",
			EntryKind:             EntryKindPayCode,
			RequiresDelegatedAuth: true,
			RequiredUserAgent:     UserAgentPurposeSmartCampus,
			RequiredHeaderNames:   append([]string(nil), headers...),
			DataStatus:            status,
		},
	}
}

func resolveEntryURL(entryID string, demoMode bool) (string, error) {
	switch entryID {
	case EntryIDLuoJiaECard:
		if demoMode {
			return DemoECardEntryURL, nil
		}
		return PublicCASEntryURL, nil
	case EntryIDPayCode:
		if demoMode {
			return DemoPayCodeURL, nil
		}
		return PublicCASEntryURL, nil
	default:
		return "", ErrInvalid
	}
}

func sessionHeaders(entryURL string, demoMode bool) map[string]string {
	requestedWith := ProductionRequestedWith
	if demoMode {
		requestedWith = DemoRequestedWith
	}
	return map[string]string{
		HeaderRequestedWith: requestedWith,
		HeaderReferer:       entryURL,
	}
}

func userAgent(demoMode bool) string {
	if demoMode {
		return DemoUserAgent
	}
	return GovernedSmartCampusUserAgent
}

func cookieNames() []string {
	return []string{CookieNameCASTGC, CookieNameJSESSIONID}
}
