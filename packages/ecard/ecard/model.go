// Package ecard 是珞珈 E 卡/付款码双入口包的领域模型：5 个 Capability 的
// 文档、请求与校验。纯 stdlib，无内核依赖——包将独立 repo 化。
//
// 安全边界：hosted wasm guest 永不持有真实凭据（CAS Cookie 等）与密钥——
// 真实凭据留给后续签名的学校认证包（阶段 2c）。本包只承载演示材料
// （demo: 前缀）与不含秘密的入口/会话描述；看起来像真实凭据的材料
// （CASTGC / CAS 主机 / TGT-/ST- 票据）一律 fail-closed 拒绝。凭据文档
// 是个人作用域数据（宿主从治理上下文注入 UserID）；入口目录是包内常量，
// 显式标注 non_authoritative（演示形态）。
package ecard

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// errInvalidArgument 是请求校验失败的包内哨兵：app 层映射为稳定错误码。
var errInvalidArgument = errors.New("invalid argument")

// 状态与上限常量。
const (
	StatusActive   = "active"
	StatusRevoked  = "revoked"
	StatusExpired  = "expired"
	StatusNotFound = "not_found"

	EntryIDEcard = "luojia_ecard"
	EntryIDPay   = "ecard_paycode"

	KindDemoHandle = "demo_handle"

	// MaxMaterialRunes 是演示材料字符上限；MaxTTL 是凭据有效期上限。
	MaxMaterialRunes = 256
	MaxTTL           = 7 * 24 * time.Hour

	// DemoMaterialPrefix 是演示材料强制前缀；无此前缀视为疑似真实凭据。
	DemoMaterialPrefix = "demo:"

	// MaxIDLength 约束稳定标识长度。
	MaxIDLength = 128
)

// 集合名（个人作用域文档）。
const (
	CredentialsCollection = "credentials"
)

// 文档：演示凭据（个人作用域，不含真实秘密）。
type (
	// Credential 是个人作用域的演示凭据文档。
	Credential struct {
		CredentialID string `json:"credential_id"`
		Kind         string `json:"kind"`
		Status       string `json:"status"`
		Fingerprint  string `json:"fingerprint"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
		RevokedAt    string `json:"revoked_at,omitempty"`
	}
)

// Entry 是双入口目录中的一项：不含 Cookie 值或付款码载荷。
type Entry struct {
	ID                    string   `json:"id"`
	Title                 string   `json:"title"`
	EntryKind             string   `json:"entry_kind"`
	EntryURL              string   `json:"entry_url"`
	RequiresDelegatedAuth bool     `json:"requires_delegated_auth"`
	RequiredUserAgent     string   `json:"required_user_agent"`
	RequiredHeaders       []Header `json:"required_headers"`
}

// Header 是 WebView 会话要求的一个请求头（名称 + 用途）。
type Header struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

// SessionPlan 是客户端 WebView 应应用的会话描述：不含任何凭据值。
type SessionPlan struct {
	EntryID               string   `json:"entry_id"`
	EntryURL              string   `json:"entry_url"`
	RequiredUserAgent     string   `json:"required_user_agent"`
	RequiredHeaders       []Header `json:"required_headers"`
	CredentialFingerprint string   `json:"credential_fingerprint"`
	ExpiresAt             string   `json:"expires_at"`
}

// DataStatus 是入口目录的治理状态：演示形态显式非权威（不是治理失败，
// 而是数据源本身的标注）。
type DataStatus struct {
	State          string `json:"state"`
	Source         string `json:"source"`
	Authoritative  bool   `json:"authoritative"`
	Complete       bool   `json:"complete"`
	SourceRevision string `json:"source_revision"`
	ValidUntil     string `json:"valid_until"`
}

// EffectiveStatus 推导凭据有效状态：active 且已过 ExpiresAt → expired。
func (c Credential) EffectiveStatus(now time.Time) string {
	switch c.Status {
	case StatusRevoked:
		return StatusRevoked
	case StatusActive:
		expiresAt, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil || !now.Before(expiresAt) {
			return StatusExpired
		}
		return StatusActive
	default:
		return c.Status
	}
}

// ---- 请求 ----

type (
	// EntriesListRequest 列出双入口目录。
	EntriesListRequest struct{}

	// SessionPrepareRequest 校验凭据可用并生成 WebView 会话描述。
	SessionPrepareRequest struct {
		EntryID      string `json:"entry_id"`
		CredentialID string `json:"credential_id"`
	}

	// CredentialsPutRequest 存入演示凭据材料（demo: 前缀）。
	CredentialsPutRequest struct {
		Kind     string `json:"kind"`
		Material string `json:"material"`
		TTLHours int    `json:"ttl_hours"`
	}

	// CredentialsRevokeRequest 撤销本人凭据。
	CredentialsRevokeRequest struct {
		CredentialID string `json:"credential_id"`
	}

	// CredentialsStatusRequest 查询凭据存在性（不含秘密）。
	CredentialsStatusRequest struct {
		CredentialID string `json:"credential_id"`
	}
)

// DefaultTTLHours / MaxTTLHours 是凭据默认与最大有效期（小时）。
const (
	DefaultTTLHours = 24
	MaxTTLHours     = int(MaxTTL / time.Hour)
)

// NormalizeAndValidate 校验 entry/credential 必填。
func (r *SessionPrepareRequest) NormalizeAndValidate() error {
	r.EntryID = strings.TrimSpace(r.EntryID)
	r.CredentialID = strings.TrimSpace(r.CredentialID)
	if !validEntryID(r.EntryID) || !validStableID(r.CredentialID) {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 校验 kind 与演示材料，缺省 TTL=24h。
func (r *CredentialsPutRequest) NormalizeAndValidate() error {
	r.Kind = strings.TrimSpace(r.Kind)
	r.Material = strings.TrimSpace(r.Material)
	if r.TTLHours == 0 {
		r.TTLHours = DefaultTTLHours
	}
	if r.Kind != KindDemoHandle || !validDemoMaterial(r.Material) ||
		r.TTLHours < 1 || r.TTLHours > MaxTTLHours {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 校验 credential_id 必填。
func (r *CredentialsRevokeRequest) NormalizeAndValidate() error {
	r.CredentialID = strings.TrimSpace(r.CredentialID)
	if !validStableID(r.CredentialID) {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 校验 credential_id 必填。
func (r *CredentialsStatusRequest) NormalizeAndValidate() error {
	r.CredentialID = strings.TrimSpace(r.CredentialID)
	if !validStableID(r.CredentialID) {
		return errInvalidArgument
	}
	return nil
}

// ---- 校验辅助 ----

func validStableID(value string) bool {
	return value != "" && len(value) <= MaxIDLength && utf8.ValidString(value)
}

func validEntryID(value string) bool {
	return value == EntryIDEcard || value == EntryIDPay
}

// validDemoMaterial 强制 demo: 前缀并拒绝疑似真实凭据的材料
// （CASTGC / CAS 主机 / TGT-/ST- 票据）——fail-closed。
func validDemoMaterial(value string) bool {
	if value == "" || len(value) > MaxMaterialRunes*4 || !utf8.ValidString(value) {
		return false
	}
	if !strings.HasPrefix(value, DemoMaterialPrefix) {
		return false
	}
	return !looksLikeRealMaterial(value)
}

// looksLikeRealMaterial 检测疑似真实 CAS 凭据的字面特征。
func looksLikeRealMaterial(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "castgc") || strings.Contains(lower, "cas.whu.edu.cn") {
		return true
	}
	return strings.Contains(value, "TGT-") || strings.Contains(value, "ST-")
}

// FingerprintOf 由材料派生非可逆指纹（材料本身不落库）。hosted guest 不持有
// 真实凭据，这里只对演示材料给出可展示的短摘要。
func FingerprintOf(material string) string {
	runes := []rune(material)
	if len(runes) > 12 {
		runes = runes[:12]
	}
	return string(runes) + "…"
}

// DemoEntries 返回演示双入口目录（显式 non_authoritative 标注；入口 URL 为
// demo.invalid 保留域，不可路由，真实入口留给阶段 2c 的学校认证包）。
func DemoEntries(now time.Time) ([]Entry, DataStatus) {
	headers := []Header{
		{Name: "X-Requested-With", Purpose: "智能校园 WebView 标识"},
		{Name: "Referer", Purpose: "入口来源校验"},
	}
	userAgent := "SmartCampus/WHU (purpose=smart_campus; governed-by=ailuo)"
	entries := []Entry{
		{
			ID: EntryIDEcard, Title: "珞珈E卡", EntryKind: "luojia_ecard",
			EntryURL:              "https://demo.invalid/luojia-ecard",
			RequiresDelegatedAuth: true, RequiredUserAgent: userAgent,
			RequiredHeaders: headers,
		},
		{
			ID: EntryIDPay, Title: "付款码", EntryKind: "paycode",
			EntryURL:              "https://demo.invalid/ecard-paycode",
			RequiresDelegatedAuth: true, RequiredUserAgent: userAgent,
			RequiredHeaders: headers,
		},
	}
	status := DataStatus{
		State:          "non_authoritative",
		Source:         "demo-fixture-not-zhihui-luojia",
		Authoritative:  false,
		Complete:       true,
		SourceRevision: "ecard-catalog-v1",
		ValidUntil:     now.UTC().Add(24 * time.Hour).Format(time.RFC3339),
	}
	return entries, status
}
