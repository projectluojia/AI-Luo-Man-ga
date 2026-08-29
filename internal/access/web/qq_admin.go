package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type qqSettingsInput struct {
	Generation            uint64   `json:"generation"`
	Enabled               bool     `json:"enabled"`
	WSURL                 string   `json:"ws_url"`
	BotQQID               string   `json:"bot_qq_id"`
	AllowedGroupIDs       []string `json:"allowed_group_ids"`
	AllowedPrivateUserIDs []string `json:"allowed_private_user_ids"`
}

func (s *Server) getQQAccess(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeQQAdmin(writer, request) {
		return
	}
	settings, runtimeStatus, err := s.qqAccessAdmin.Snapshot(request.Context())
	if err != nil {
		observe.Error(request.Context(), "读取 QQ Access 配置失败", err)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "qq_access_unavailable", "message": "QQ 接入配置暂时不可用"})
		return
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"settings": settings, "runtime": runtimeStatus})
}

func (s *Server) updateQQAccess(writer http.ResponseWriter, request *http.Request) {
	if !s.authorizeQQAdmin(writer, request) {
		return
	}
	var input qqSettingsInput
	if !access.DecodeJSONBody(writer, request, &input, 64<<10) {
		return
	}
	if input.Generation == 0 {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_generation", "message": "配置代数必须大于 0"})
		return
	}
	replacement := qqsettings.Settings{
		AppID: s.appID, Enabled: input.Enabled, WSURL: strings.TrimSpace(input.WSURL), BotQQID: strings.TrimSpace(input.BotQQID),
		AllowedGroupIDs: input.AllowedGroupIDs, AllowedPrivateUserIDs: input.AllowedPrivateUserIDs,
	}
	updated, runtimeStatus, err := s.qqAccessAdmin.Update(request.Context(), input.Generation, replacement)
	if err != nil {
		switch {
		case errors.Is(err, qqsettings.ErrInvalid):
			access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_qq_access_settings", "message": "QQ 接入配置不合法"})
		case errors.Is(err, qqsettings.ErrConflict):
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "qq_access_conflict", "message": "配置已被其他操作更新，请刷新后重试"})
		default:
			observe.Error(request.Context(), "更新 QQ Access 配置失败", err)
			access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "qq_access_unavailable", "message": "QQ 接入配置暂时不可用"})
		}
		return
	}
	observe.Info(request.Context(), "QQ Access 配置已热更新",
		observe.StringAttr("app_id", s.appID),
		observe.BoolAttr("enabled", updated.Enabled),
		observe.IntAttr("allowed_group_count", len(updated.AllowedGroupIDs)),
		observe.IntAttr("allowed_private_user_count", len(updated.AllowedPrivateUserIDs)),
	)
	access.WriteJSON(writer, http.StatusOK, map[string]any{"settings": updated, "runtime": runtimeStatus})
}

func (s *Server) authorizeQQAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if s.qqAccessAdmin == nil {
		http.NotFound(writer, request)
		return false
	}
	if !loopbackAdminRequest(request) {
		access.WriteJSON(writer, http.StatusForbidden, map[string]string{"code": "local_admin_required", "message": "QQ 接入配置只允许在本机管理"})
		return false
	}
	return true
}
