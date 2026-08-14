package web

import (
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// AuthenticatedWebIdentity 是可信 Web 认证层提供的平台身份。请求正文中的
// user_id、user_name 与 session_id 不得用于填充本结构。
type AuthenticatedWebIdentity struct {
	PlatformSpaceID   string
	PlatformUserID    string
	PlatformSessionID string
}

// WebAuthenticator 从服务端可信登录态解析 Web 平台身份。
type WebAuthenticator interface {
	Authenticate(*http.Request) (AuthenticatedWebIdentity, error)
}

// ServerOption 配置 Web Access 的可选生产依赖。
type ServerOption func(*Server)

// WithWebAuthenticator 注入可信 Web 登录态解析器。未注入时聊天创建入口
// 返回 401，不降级为匿名用户。
func WithWebAuthenticator(authenticator WebAuthenticator) ServerOption {
	return func(server *Server) {
		server.webAuthenticator = authenticator
	}
}

func (s *Server) authenticateWeb(writer http.ResponseWriter, request *http.Request) (AuthenticatedWebIdentity, bool) {
	if s.webAuthenticator == nil {
		access.WriteJSON(writer, http.StatusUnauthorized, map[string]string{
			"code": "authentication_required", "message": "请先登录后再发起对话",
		})
		return AuthenticatedWebIdentity{}, false
	}
	resolved, err := s.webAuthenticator.Authenticate(request)
	if err != nil {
		observe.Warn(request.Context(), "Web 用户认证失败")
		access.WriteJSON(writer, http.StatusUnauthorized, map[string]string{
			"code": "authentication_required", "message": "请先登录后再发起对话",
		})
		return AuthenticatedWebIdentity{}, false
	}
	if identity.ValidateBindingKey(s.appID, "web", resolved.PlatformSpaceID, resolved.PlatformUserID) != nil ||
		identity.ValidatePlatformUserID(resolved.PlatformSessionID) != nil {
		observe.Error(request.Context(), "Web 认证层返回非法身份上下文", access.ErrIdentityContextInvalid)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{
			"code": "internal_error", "message": "身份认证服务异常",
		})
		return AuthenticatedWebIdentity{}, false
	}
	return resolved, true
}
