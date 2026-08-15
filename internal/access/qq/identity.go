package qq

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

// IdentityProvisioner 是白名单通过后、进入 Hub 前所需的身份开通端口。
type IdentityProvisioner interface {
	EnsureQQIdentity(context.Context, access.InboundMessage) error
}

// IdentityController 是 QQ Access 使用的身份控制面窄端口。
type IdentityController interface {
	CreateUser(context.Context, string) (identity.User, error)
	SetMembership(context.Context, identity.AppMembership) error
	BindExternalIdentity(context.Context, identity.ExternalIdentity) error
	ResolveIdentity(context.Context, string, string, string, string) (identity.IdentityContext, error)
}

// Provisioner 为已获准的 QQ 用户幂等创建内部身份、App 成员关系和空间绑定。
type Provisioner struct {
	identities IdentityController
}

func NewProvisioner(identities IdentityController) (*Provisioner, error) {
	if identities == nil {
		return nil, errors.New("qq identity provisioner configuration is incomplete")
	}
	return &Provisioner{identities: identities}, nil
}

// EnsureQQIdentity 只接受已经由 QQ Adapter 规范化的 QQ 消息。成员关系先于
// 外部身份绑定写入，避免并发消息看到一个尚未具备 App 成员关系的半成品身份。
func (p *Provisioner) EnsureQQIdentity(ctx context.Context, message access.InboundMessage) error {
	if message.Platform != "qq" || identity.ValidateAppID(message.AppID) != nil ||
		identity.ValidatePlatformSpaceID(message.PlatformSpaceID) != nil ||
		identity.ValidatePlatformUserID(message.PlatformUserID) != nil {
		return identity.ErrInvalid
	}
	userID := qqUserID(message.AppID, message.PlatformUserID)
	resolved, resolveErr := p.identities.ResolveIdentity(ctx, message.AppID, "qq", message.PlatformSpaceID, message.PlatformUserID)
	userExists := resolveErr == nil
	if resolveErr == nil {
		if resolved.UserID != userID {
			return identity.ErrAlreadyBound
		}
		if resolved.Membership != nil {
			return nil
		}
	} else if !errors.Is(resolveErr, identity.ErrNotFound) {
		return fmt.Errorf("resolve qq identity: %w", resolveErr)
	}
	if !userExists {
		if _, err := p.identities.CreateUser(ctx, userID); err != nil && !errors.Is(err, identity.ErrConflict) {
			return fmt.Errorf("create qq user: %w", err)
		}
	}
	if err := p.identities.SetMembership(ctx, identity.AppMembership{AppID: message.AppID, UserID: userID}); err != nil {
		return fmt.Errorf("set qq membership: %w", err)
	}
	if err := p.identities.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: message.AppID, Platform: "qq", PlatformSpaceID: message.PlatformSpaceID,
		PlatformUserID: message.PlatformUserID, UserID: userID,
	}); err != nil {
		return fmt.Errorf("bind qq identity: %w", err)
	}
	return nil
}

func qqUserID(appID, platformUserID string) string {
	digest := sha256.Sum256([]byte(appID + "\x00qq\x00" + platformUserID))
	return fmt.Sprintf("qq-user-v1-%x", digest[:])
}
