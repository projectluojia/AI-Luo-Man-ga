package ecard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// ToolSpecs 返回 E 卡原子 Tool 规格；与 Capability Schema 保持同一来源。
func ToolSpecs() []registry.ToolSpec {
	return []registry.ToolSpec{
		{
			ID:              EntriesListToolID,
			Version:         "1.0.0",
			Description:     "List governed LuoJia e-card and payment-code WebView entries.",
			InputSchemaJSON: EntriesListInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
		{
			ID:              SessionPrepareToolID,
			Version:         "1.0.0",
			Description:     "Prepare a WebView session plan after a delegated credential handle is stored.",
			InputSchemaJSON: SessionPrepareInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
		{
			ID:                   CredentialsPutToolID,
			Version:              "1.0.0",
			Description:          "Store delegated CAS cookie or demo handle ciphertext for the current user.",
			InputSchemaJSON:      CredentialsPutInputSchemaJSON,
			SideEffect:           registry.SideEffectWrite,
			RequiresConfirmation: true,
		},
		{
			ID:                   CredentialsRevokeToolID,
			Version:              "1.0.0",
			Description:          "Revoke the stored delegated e-card credential ciphertext.",
			InputSchemaJSON:      CredentialsRevokeInputSchemaJSON,
			SideEffect:           registry.SideEffectWrite,
			RequiresConfirmation: true,
		},
		{
			ID:              CredentialsStatusToolID,
			Version:         "1.0.0",
			Description:     "Return whether a delegated e-card credential exists, without secret values.",
			InputSchemaJSON: CredentialsStatusInputSchemaJSON,
			SideEffect:      registry.SideEffectRead,
		},
	}
}

// ToolRegistrations 返回由统一存储与密钥配置驱动的 Tool 注册项。
func ToolRegistrations(cfg Config) []registry.ToolRegistration {
	handlers := ToolHandlers(cfg)
	registrations := make([]registry.ToolRegistration, 0, len(ToolSpecs()))
	for _, spec := range ToolSpecs() {
		registrations = append(registrations, registry.ToolRegistration{Spec: spec, Handler: handlers[spec.ID]})
	}
	return registrations
}

// ToolHandlers 构造原子 Tool 处理器。
func ToolHandlers(cfg Config) map[string]registry.Handler {
	return map[string]registry.Handler{
		EntriesListToolID:       listEntriesHandler(cfg),
		SessionPrepareToolID:    prepareSessionHandler(cfg),
		CredentialsPutToolID:    putCredentialHandler(cfg),
		CredentialsRevokeToolID: revokeCredentialHandler(cfg),
		CredentialsStatusToolID: statusCredentialHandler(cfg),
	}
}

type prepareInput struct {
	EntryID          string `json:"entry_id"`
	CredentialHandle string `json:"credential_handle"`
}

type putInput struct {
	Kind               string `json:"kind"`
	CredentialMaterial string `json:"credential_material"`
	ExpiresAt          string `json:"expires_at"`
}

type revokeInput struct {
	Kind             string `json:"kind"`
	CredentialHandle string `json:"credential_handle"`
}

type statusInput struct {
	Kind string `json:"kind"`
}

func listEntriesHandler(cfg Config) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := identity.ValidateAppID(request.AppID); err != nil {
			return nil, errors.Join(registry.ErrPermissionDenied, ErrInvalid)
		}
		var input struct{}
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		status := catalogStatus(cfg.clock()(), cfg.DemoMode)
		entries := dualEntries(status)
		observe.Info(ctx, "E 卡双入口目录已返回",
			observe.StringAttr("app_id", request.AppID),
			observe.IntAttr("entry_count", len(entries)),
			observe.BoolAttr("authoritative", false),
			observe.BoolAttr("demo_mode", cfg.DemoMode),
		)
		return json.Marshal(map[string]any{"entries": entries, "data_status": status})
	}
}

func prepareSessionHandler(cfg Config) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(cfg.Store); err != nil {
			return nil, err
		}
		var input prepareInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		kind, err := kindFromHandle(input.CredentialHandle)
		if err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		if cfg.DemoMode && kind != KindDemoHandle {
			return nil, errors.Join(contracts.ErrDataUntrusted, ErrProductionMaterial)
		}
		if !cfg.DemoMode && kind != KindCASCookie {
			return nil, errors.Join(contracts.ErrDataUntrusted, ErrDemoMaterial)
		}
		record, err := cfg.Store.GetActiveECardCredential(ctx, request.AppID, request.UserID, kind)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, failClosedUnavailable(cfg, "missing delegated credential")
			}
			return nil, err
		}
		now := cfg.clock()().UTC()
		if !record.ExpiresAt.After(now) {
			return nil, errors.Join(contracts.ErrDataExpired, ErrExpired)
		}
		key, err := cfg.aesKey()
		if err != nil {
			return nil, failClosedUnavailable(cfg, "credential key missing")
		}
		plain, err := decryptMaterial(key, record.Nonce, record.Ciphertext, record.AppID, record.UserID, record.Kind)
		clearBytes(key)
		if err != nil {
			return nil, failClosedUnavailable(cfg, "credential ciphertext is unreadable")
		}
		// 仅用于完整性校验，随后立即清零，绝不写入响应。
		clearBytes(plain)
		resolvedURL, err := resolveEntryURL(input.EntryID, cfg.DemoMode)
		if err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		status := catalogStatus(now, cfg.DemoMode)
		status.ValidUntil = record.ExpiresAt.UTC()
		plan := SessionPlan{
			EntryID:     input.EntryID,
			EntryURL:    resolvedURL,
			UserAgent:   userAgent(cfg.DemoMode),
			Headers:     sessionHeaders(resolvedURL, cfg.DemoMode),
			CookieNames: cookieNames(),
			ExpiresAt:   record.ExpiresAt.UTC(),
			DataStatus:  status,
		}
		observe.Info(ctx, "E 卡 WebView 会话计划已生成",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("entry_id", input.EntryID),
			observe.StringAttr("credential_kind", kind),
			observe.BoolAttr("authoritative", false),
		)
		return json.Marshal(plan)
	}
}

func putCredentialHandler(cfg Config) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(cfg.Store); err != nil {
			return nil, err
		}
		var input putInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		material := input.CredentialMaterial
		input.CredentialMaterial = ""
		if err := validatePutMaterial(input.Kind, material, cfg.DemoMode, cfg.Production); err != nil {
			clearString(&material)
			return nil, err
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, input.ExpiresAt)
		if err != nil {
			expiresAt, err = time.Parse(time.RFC3339, input.ExpiresAt)
		}
		if err != nil {
			clearString(&material)
			return nil, errors.Join(registry.ErrSchemaValidation, ErrInvalid)
		}
		now := cfg.clock()().UTC()
		expiresAt = expiresAt.UTC()
		if !expiresAt.After(now) || expiresAt.Sub(now) > MaxCredentialTTL {
			clearString(&material)
			return nil, errors.Join(registry.ErrSchemaValidation, ErrInvalid)
		}
		key, err := cfg.aesKey()
		if err != nil {
			clearString(&material)
			return nil, failClosedUnavailable(cfg, "credential key missing")
		}
		plain := []byte(material)
		clearString(&material)
		nonce, ciphertext, err := encryptMaterial(key, request.AppID, request.UserID, input.Kind, plain)
		clearBytes(key, plain)
		if err != nil {
			return nil, err
		}
		record, err := cfg.Store.PutECardCredential(ctx, CredentialRecord{
			AppID:      request.AppID,
			UserID:     request.UserID,
			Kind:       input.Kind,
			Nonce:      nonce,
			Ciphertext: ciphertext,
			CreatedAt:  now,
			ExpiresAt:  expiresAt,
		})
		clearBytes(nonce, ciphertext)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "E 卡委托凭据密文已写入",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("credential_kind", input.Kind),
			observe.BoolAttr("demo_mode", cfg.DemoMode),
		)
		return json.Marshal(CredentialPutResult{
			CredentialHandle: handleForKind(record.Kind),
			Kind:             record.Kind,
			HasCredential:    true,
			ExpiresAt:        record.ExpiresAt.UTC(),
		})
	}
}

func revokeCredentialHandler(cfg Config) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(cfg.Store); err != nil {
			return nil, err
		}
		var input revokeInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		kind := strings.TrimSpace(input.Kind)
		if kind == "" && input.CredentialHandle != "" {
			parsed, err := kindFromHandle(input.CredentialHandle)
			if err != nil {
				return nil, errors.Join(registry.ErrSchemaValidation, err)
			}
			kind = parsed
		}
		if !validKind(kind) {
			return nil, errors.Join(registry.ErrSchemaValidation, ErrInvalid)
		}
		if err := cfg.Store.RevokeECardCredential(ctx, request.AppID, request.UserID, kind, cfg.clock()().UTC()); err != nil {
			return nil, err
		}
		observe.Info(ctx, "E 卡委托凭据已撤销",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("credential_kind", kind),
		)
		return json.Marshal(map[string]any{"revoked": true, "kind": kind, "has_credential": false})
	}
}

func statusCredentialHandler(cfg Config) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(cfg.Store); err != nil {
			return nil, err
		}
		var input statusInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		kinds := []string{KindCASCookie, KindDemoHandle}
		if input.Kind != "" {
			if !validKind(input.Kind) {
				return nil, errors.Join(registry.ErrSchemaValidation, ErrInvalid)
			}
			kinds = []string{input.Kind}
		}
		now := cfg.clock()().UTC()
		for _, kind := range kinds {
			meta, err := cfg.Store.GetECardCredentialMeta(ctx, request.AppID, request.UserID, kind)
			if err != nil {
				return nil, err
			}
			if meta.Present && !meta.Revoked && meta.ExpiresAt.After(now) {
				expires := meta.ExpiresAt.UTC()
				return json.Marshal(CredentialStatusResult{
					HasCredential:    true,
					Kind:             meta.Kind,
					CredentialHandle: handleForKind(meta.Kind),
					ExpiresAt:        &expires,
				})
			}
		}
		return json.Marshal(CredentialStatusResult{HasCredential: false})
	}
}

func decode(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := jsonutil.DecodeStrict(payload, target); err != nil {
		return errors.Join(registry.ErrSchemaValidation, err)
	}
	return nil
}

func ensureStore(store Store) error {
	if store == nil {
		return errors.New("ecard store is unavailable")
	}
	return nil
}

func failClosedUnavailable(_ Config, _ string) error {
	return errors.Join(contracts.ErrDataUnavailable, ErrNotFound)
}

func clearString(value *string) {
	if value == nil {
		return
	}
	*value = strings.Repeat("\x00", len(*value))
	*value = ""
}
