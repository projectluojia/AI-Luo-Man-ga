// Package ecard 装配珞珈 E 卡 Service，并仅通过 Registry 暴露受治理 Capability。
package ecard

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	ecardtool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

const (
	ServiceID = ecardtool.ServiceID

	EntriesListCapabilityID       = ecardtool.EntriesListToolID
	SessionPrepareCapabilityID    = ecardtool.SessionPrepareToolID
	CredentialsPutCapabilityID    = ecardtool.CredentialsPutToolID
	CredentialsRevokeCapabilityID = ecardtool.CredentialsRevokeToolID
	CredentialsStatusCapabilityID = ecardtool.CredentialsStatusToolID
)

// Service 是 E 卡业务的薄组合层，状态全部由传入的存储端口与密钥配置管理。
type Service struct{ cfg ecardtool.Config }

// NewService 返回 E 卡 Service；密钥会被复制，调用方可立即清零入参。
func NewService(cfg ecardtool.Config) *Service {
	if len(cfg.Key) > 0 {
		key := make([]byte, len(cfg.Key))
		copy(key, cfg.Key)
		cfg.Key = key
	}
	return &Service{cfg: cfg}
}

// ToolIDs 返回 E 卡 Service 声明的 Tool 依赖。
func ToolIDs() []string {
	specs := ecardtool.ToolSpecs()
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	return ids
}

// CapabilityIDs 返回对外暴露的 Capability 标识。
func CapabilityIDs() []string {
	return []string{
		EntriesListCapabilityID,
		SessionPrepareCapabilityID,
		CredentialsPutCapabilityID,
		CredentialsRevokeCapabilityID,
		CredentialsStatusCapabilityID,
	}
}

// ServiceSpec 返回 E 卡 Service 的注册元数据。
func ServiceSpec() registry.ServiceSpec {
	return registry.ServiceSpec{
		ID:               ServiceID,
		Version:          "1.0.0",
		Description:      "Governed LuoJia e-card and payment-code WebView session descriptors with encrypted delegated credentials.",
		ToolDependencies: ToolIDs(),
	}
}

// Register 是与其他 L3 Service 一致的便捷装配入口。
func Register(reg *registry.Registry, service *Service) error {
	if service == nil {
		return registry.ErrInvalidSpec
	}
	return service.Register(reg)
}

// Register 原子注册 E 卡 Tool、Service 与 Capability。
func (s *Service) Register(reg *registry.Registry) error {
	if s == nil || reg == nil || s.cfg.Store == nil {
		return registry.ErrInvalidSpec
	}
	handlers := ecardtool.ToolHandlers(s.cfg)
	capability := func(id, name, description, schema, sideEffect string, confirm bool) struct {
		Spec    registry.CapabilitySpec
		Handler registry.Handler
	} {
		return struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			Spec: registry.CapabilitySpec{
				ID:                   id,
				Version:              "1.0.0",
				Name:                 name,
				Description:          description,
				ServiceID:            ServiceID,
				InputSchemaJSON:      schema,
				SideEffect:           sideEffect,
				RequiresConfirmation: confirm,
				ToolID:               id,
			},
			Handler: handlers[id],
		}
	}
	read := registry.SideEffectRead
	write := registry.SideEffectWrite
	return reg.RegisterBatch(ecardtool.ToolRegistrations(s.cfg), []registry.ServiceRegistration{{
		Spec: ServiceSpec(),
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			EntriesListCapabilityID: capability(
				EntriesListCapabilityID, "列出珞珈E卡入口",
				"List dual LuoJia e-card and payment-code WebView entries.",
				ecardtool.EntriesListInputSchemaJSON, read, false,
			),
			SessionPrepareCapabilityID: capability(
				SessionPrepareCapabilityID, "准备E卡 WebView 会话",
				"Return a WebView session plan after a delegated credential handle is stored. Never returns cookie values.",
				ecardtool.SessionPrepareInputSchemaJSON, read, false,
			),
			CredentialsPutCapabilityID: capability(
				CredentialsPutCapabilityID, "保存E卡委托凭据",
				"Encrypt and store a delegated CAS cookie or demo handle for the current user.",
				ecardtool.CredentialsPutInputSchemaJSON, write, true,
			),
			CredentialsRevokeCapabilityID: capability(
				CredentialsRevokeCapabilityID, "撤销E卡委托凭据",
				"Revoke stored delegated e-card credential ciphertext.",
				ecardtool.CredentialsRevokeInputSchemaJSON, write, true,
			),
			CredentialsStatusCapabilityID: capability(
				CredentialsStatusCapabilityID, "查询E卡凭据状态",
				"Return whether a delegated e-card credential exists, without secret values.",
				ecardtool.CredentialsStatusInputSchemaJSON, read, false,
			),
		},
	}})
}
