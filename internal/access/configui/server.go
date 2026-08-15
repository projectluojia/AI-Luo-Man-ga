package configui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	config "github.com/projectluojia/AI-Luo-Man-ga/internal/controlplane/config"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const DefaultAddress = "127.0.0.1:9178"

//go:embed static/*
var staticFiles embed.FS

type SaveInput = config.SaveInput

var (
	ErrConflict = config.ErrConflict
	ErrInvalid  = config.ErrInvalid
)

// Server 是独立于内核就绪状态的本机配置 WebUI。
type Server struct {
	service *config.Service
}

func NewServer(service *config.Service) (*Server, error) {
	if service == nil {
		return nil, errors.New("local configuration service is nil")
	}
	return &Server{service: service}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	mux.HandleFunc("PUT /api/v1/config", s.updateConfig)
	static, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return observe.HTTPMiddleware("local_config", access.SecurityHeaders(mux))
}

// Run 在本机地址提供配置页面，ctx 取消时有界关闭。
func (s *Server) Run(ctx context.Context, address string) error {
	if !validListenAddress(address) {
		return errors.New("local configuration server requires a loopback address")
	}
	server := &http.Server{
		Addr: address, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func validListenAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(strings.Trim(host, "[]"))
	return parsed != nil && parsed.IsLoopback()
}

func (s *Server) getConfig(writer http.ResponseWriter, request *http.Request) {
	if !authorizeLocal(writer, request) {
		return
	}
	access.WriteJSON(writer, http.StatusOK, s.service.Snapshot())
}

func (s *Server) updateConfig(writer http.ResponseWriter, request *http.Request) {
	if !authorizeLocal(writer, request) {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 256<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input SaveInput
	if err := decoder.Decode(&input); err != nil || jsonutil.EnsureEOF(decoder) != nil {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "配置内容不是有效的 JSON 对象"})
		return
	}
	snapshot, err := s.service.Save(input)
	if err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "configuration_conflict", "message": "配置已更新，请刷新页面后重试"})
		case errors.Is(err, ErrInvalid):
			access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_configuration", "message": "请检查模型、密钥和 QQ 配置"})
		default:
			observe.Error(request.Context(), "保存本机配置失败", err)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "configuration_write_failed", "message": "配置保存失败"})
		}
		return
	}
	observe.Info(request.Context(), "本机配置已保存，正在应用",
		observe.Int64Attr("config_revision", int64(snapshot.Settings.Revision)),
		observe.BoolAttr("qq_enabled", snapshot.Settings.QQEnabled),
	)
	access.WriteJSON(writer, http.StatusOK, snapshot)
}

func authorizeLocal(writer http.ResponseWriter, request *http.Request) bool {
	remoteHost, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		remoteHost = request.RemoteAddr
	}
	remote := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remote == nil || !remote.IsLoopback() || !loopbackHost(request.Host) {
		access.WriteJSON(writer, http.StatusForbidden, map[string]string{"code": "local_admin_required", "message": "配置页面只允许在本机访问"})
		return false
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host != request.Host {
			access.WriteJSON(writer, http.StatusForbidden, map[string]string{"code": "same_origin_required", "message": "配置请求来源无效"})
			return false
		}
	}
	return true
}

func loopbackHost(value string) bool {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = strings.Trim(value, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}
