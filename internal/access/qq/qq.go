// Package qq 提供 QQ 平台适配器（OneBot v11 WebSocket 客户端）：把 QQ 消息
// 事件规范化为标准 InboundMessage 经 Hub 入站，订阅 Echo 事件并把最终回复
// 回发。适配器不创建 Echo 之外的任何状态，不解析身份（身份绑定由控制面
// identity-bind 完成，未绑定身份的消息被安全拒绝并回发公共错误）。
package qq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	defaultDialTimeout    = 10 * time.Second
	defaultReconnectDelay = 5 * time.Second
	defaultRunTimeout     = 3 * time.Minute
	maxReplyChunk         = 4000 // QQ 单条消息安全长度上限
)

var cqCodePattern = regexp.MustCompile(`\[CQ:[^\]]*\]`)

// Config 装配 QQ 适配器。
type Config struct {
	AppID          string
	WSURL          string
	Token          string
	BotQQID        string // 机器人 QQ 号：戳一戳识别 target_id、@提及匹配
	DialTimeout    time.Duration
	ReconnectDelay time.Duration
	RunTimeout     time.Duration
}

// EchoStarter 是适配器所需的 Echo 创建端口（*kernel/echo.Orchestrator 满足）。
type EchoStarter interface {
	CreateIdempotent(context.Context, kernelecho.RunRequest) (string, bool, error)
}

// EchoReader 是持久化事件重放端口（*sqlite.Store 满足）：订阅后先重放既有
// 事件再等待实时事件，消除创建与订阅之间的竞态。
type EchoReader interface {
	GetEcho(context.Context, string, string) (kernelecho.Record, []kernelecho.Event, error)
}

// Adapter 是进程内 QQ 适配器：一条 OneBot 连接，消息事件循环 + 回复回发。
type Adapter struct {
	cfg          Config
	hub          *access.Hub
	events       *access.EventHub
	orchestrator EchoStarter
	reader       EchoReader

	conn   *websocket.Conn
	sendMu sync.Mutex
}

// New 构造 QQ 适配器；配置缺失时返回显式错误。
func New(cfg Config, hub *access.Hub, events *access.EventHub, orchestrator EchoStarter, reader EchoReader) (*Adapter, error) {
	if cfg.AppID == "" || cfg.WSURL == "" || hub == nil || events == nil || orchestrator == nil || reader == nil {
		return nil, errors.New("qq adapter configuration is incomplete")
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = defaultReconnectDelay
	}
	if cfg.RunTimeout == 0 {
		cfg.RunTimeout = defaultRunTimeout
	}
	return &Adapter{
		cfg: cfg, hub: hub, events: events, orchestrator: orchestrator, reader: reader,
	}, nil
}

// Run 连接 OneBot WebSocket 并处理消息；断线按 ReconnectDelay 重连，
// ctx 取消时退出。
func (a *Adapter) Run(ctx context.Context) error {
	for {
		err := a.serve(ctx)
		if ctx.Err() != nil || err == nil {
			return nil
		}
		observe.Warn(ctx, "QQ 连接中断，准备重连",
			observe.StringAttr("reason", err.Error()),
			observe.Int64Attr("reconnect_delay_ms", a.cfg.ReconnectDelay.Milliseconds()),
		)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.ReconnectDelay):
		}
	}
}

func (a *Adapter) serve(ctx context.Context) error {
	header := http.Header{}
	if a.cfg.Token != "" {
		header.Set("Authorization", "Bearer "+a.cfg.Token)
	}
	dialContext, cancel := context.WithTimeout(ctx, a.cfg.DialTimeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialContext, a.cfg.WSURL, header)
	if err != nil {
		return err
	}
	a.conn = conn
	defer func() {
		a.sendMu.Lock()
		a.conn = nil
		a.sendMu.Unlock()
		_ = conn.Close()
	}()
	observe.Info(ctx, "QQ 适配器已连接 OneBot",
		observe.StringAttr("app_id", a.cfg.AppID),
	)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			observe.Warn(ctx, "QQ 事件不是合法 JSON",
				observe.StringAttr("reason", err.Error()),
			)
			continue
		}
		if event["echo"] != nil {
			continue // 适配器自身的动作响应，忽略
		}
		if str(event, "post_type") == "notice" {
			a.handleNotice(ctx, event)
			continue
		}
		a.handleEvent(ctx, event)
	}
}

// handleEvent 处理一条 OneBot 消息事件：标准入站 → 幂等 Echo 创建 → 等待
// 终态回复 → 回发。未绑定身份等入站失败回发公共错误，不泄露内部细节。
func (a *Adapter) handleEvent(ctx context.Context, raw map[string]any) {
	inbound, mentioned := normalizeEvent(a.cfg.AppID, raw)
	if inbound == nil {
		return
	}
	if inbound.PlatformChannel == "group" && !mentioned {
		return // 群聊默认只响应 @提及，避免刷屏
	}
	intake, err := a.hub.Intake(ctx, *inbound)
	if err != nil {
		_, _, message := access.IntakePublicError(err)
		a.reply(ctx, inbound, message)
		return
	}
	echoID, _, err := a.orchestrator.CreateIdempotent(ctx, kernelecho.RunRequest{
		Message:        intake.Text,
		IdempotencyKey: inbound.IdempotencyKey,
		SessionID:      intake.SessionID,
		UserID:         intake.UserID,
		MessageID:      intake.MessageID,
		Channel:        "qq_" + inbound.PlatformChannel, // qq_group / qq_private
	})
	if err != nil {
		observe.Error(ctx, "QQ 消息创建 Echo 失败", err,
			observe.StringAttr("platform_message_id", inbound.PlatformMessageID),
		)
		a.reply(ctx, inbound, "处理失败，请稍后重试")
		return
	}
	reply := a.waitReply(ctx, echoID)
	if reply == "" {
		return
	}
	a.reply(ctx, inbound, reply)
}

// waitReply 订阅 Echo 事件并等待终态：先重放已持久化事件，再等待实时事件。
// 超时或流关闭无终态时返回空串（静默放弃，不伪造回复）。
func (a *Adapter) waitReply(ctx context.Context, echoID string) string {
	live, unsubscribe := a.events.Subscribe(a.cfg.AppID, echoID)
	defer unsubscribe()
	if _, persisted, err := a.reader.GetEcho(ctx, a.cfg.AppID, echoID); err == nil {
		for _, event := range persisted {
			if reply := terminalReply(event); reply != nil {
				return *reply
			}
		}
	} else {
		// 重放读取失败（存储临时不可用等）：记录退化并继续等待实时事件，
		// 不得静默跳过重放——Echo 若已终态，实时流关闭后将放弃回复。
		observe.Warn(ctx, "QQ 回复重放读取失败，继续等待实时事件",
			observe.StringAttr("echo_id", echoID),
			observe.StringAttr("echo_error", publicErrorCode(err)),
		)
	}
	timer := time.NewTimer(a.cfg.RunTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-timer.C:
			observe.Warn(ctx, "等待 QQ 回复超时",
				observe.StringAttr("echo_id", echoID),
			)
			return ""
		case event, open := <-live:
			if !open {
				observe.Warn(ctx, "QQ 回复事件流关闭但未收到终态回复",
					observe.StringAttr("echo_id", echoID),
				)
				return ""
			}
			if reply := terminalReply(event); reply != nil {
				return *reply
			}
		}
	}
}

// publicErrorCode 把重放读取错误归类为稳定日志字段，不输出原始错误正文。
func publicErrorCode(err error) string {
	if errors.Is(err, kernelecho.ErrEchoNotFound) {
		return "echo_not_found"
	}
	return "echo_read_failed"
}

// terminalReply 提取终态回复文本：reply.final 返回正文，run.failed 返回安全消息。
func terminalReply(event kernelecho.Event) *string {
	switch event.Type {
	case "reply.final":
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Text != "" {
			return &payload.Text
		}
	case "run.failed":
		message := "处理失败"
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Message != "" {
			message = payload.Message
		}
		return &message
	}
	return nil
}

// reply 把回复文本发送到消息来源（群消息回群，私聊回用户），按
// PlatformChannel 选择 OneBot 动作，不依赖空间 ID 是否为空。
func (a *Adapter) reply(ctx context.Context, inbound *access.InboundMessage, text string) {
	text = sanitizeReply(text)
	if text == "" {
		return
	}
	for _, chunk := range splitReply(text) {
		var action string
		params := map[string]any{"message": chunk}
		if inbound.PlatformChannel == "group" {
			action = "send_group_msg"
			params["group_id"] = onebotInt(inbound.PlatformSpaceID)
		} else {
			action = "send_private_msg"
			params["user_id"] = onebotInt(inbound.PlatformUserID)
		}
		if err := a.send(map[string]any{"action": action, "params": params}); err != nil {
			observe.Warn(ctx, "QQ 回复发送失败",
				observe.StringAttr("reason", err.Error()),
				observe.StringAttr("platform_message_id", inbound.PlatformMessageID),
			)
			return
		}
	}
}

// send 在共享连接上发送一条 OneBot 动作。
func (a *Adapter) send(payload any) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.conn == nil {
		return errors.New("qq adapter is not connected")
	}
	return a.conn.WriteJSON(payload)
}

// sanitizeReply 把回复转为 QQ 安全纯文本：移除可能注入 CQ 码的片段并折叠空行。
func sanitizeReply(text string) string {
	text = cqCodePattern.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
}

// splitReply 把超长回复按换行切成不超过 maxReplyChunk 字符的块。
func splitReply(text string) []string {
	if utf8.RuneCountInString(text) <= maxReplyChunk {
		return []string{text}
	}
	var chunks []string
	current := ""
	flush := func() {
		if current != "" {
			chunks = append(chunks, current)
			current = ""
		}
	}
	for _, line := range strings.Split(text, "\n") {
		for utf8.RuneCountInString(line) > maxReplyChunk {
			flush()
			runes := []rune(line)
			chunks = append(chunks, string(runes[:maxReplyChunk]))
			line = string(runes[maxReplyChunk:])
		}
		if current != "" && utf8.RuneCountInString(current)+utf8.RuneCountInString(line)+1 > maxReplyChunk {
			flush()
		}
		if current != "" {
			current += "\n"
		}
		current += line
	}
	flush()
	return chunks
}

// normalizeEvent 把 OneBot v11 消息事件规范化为标准入站消息；非消息事件、
// 无文本或缺失标识的消息返回 (nil, false)（忽略）。第二个返回值表示消息
// 是否 @了 bot（array 模式的 at 段匹配，或 raw_message 含对应 CQ 码）。
func normalizeEvent(appID string, raw map[string]any) (*access.InboundMessage, bool) {
	if str(raw, "post_type") != "message" {
		return nil, false
	}
	userID := str(raw, "user_id")
	messageID := str(raw, "message_id")
	if userID == "" || messageID == "" {
		return nil, false
	}
	var spaceID, channel, sessionID string
	switch str(raw, "message_type") {
	case "group":
		spaceID = str(raw, "group_id")
		channel = "group"
		sessionID = spaceID
	case "private":
		spaceID = "private" // 私聊无群号，用固定占位保证身份键合法（qq/private/QQ号）
		channel = "private"
		sessionID = userID
	default:
		return nil, false
	}
	text, mentioned := extractText(raw)
	if text == "" {
		return nil, false
	}
	return &access.InboundMessage{
		AppID:             appID,
		Platform:          "qq",
		PlatformChannel:   channel,
		PlatformSpaceID:   spaceID,
		PlatformUserID:    userID,
		PlatformMessageID: messageID,
		PlatformSessionID: sessionID,
		MessageType:       "text",
		Text:              text,
		OccurredAt:        time.Now().UTC(),
		IdempotencyKey:    messageID,
	}, mentioned
}

// extractText 从消息事件提取纯文本：优先拼接 array 模式的 text 段，退化时
// 从 raw_message 剥离 CQ 码。同时返回是否包含 at 段（任意用户）。
func extractText(raw map[string]any) (text string, mentioned bool) {
	if segments, ok := raw["message"].([]any); ok {
		var builder strings.Builder
		for _, segment := range segments {
			seg, ok := segment.(map[string]any)
			if !ok {
				continue
			}
			switch str(seg, "type") {
			case "text":
				if data, ok := seg["data"].(map[string]any); ok {
					builder.WriteString(str(data, "text"))
				}
			case "at":
				mentioned = true
			}
		}
		if text := strings.TrimSpace(builder.String()); text != "" {
			return text, mentioned
		}
	}
	return strings.TrimSpace(cqCodePattern.ReplaceAllString(str(raw, "raw_message"), "")), mentioned
}

// str 从 JSON 值读取字符串字段（OneBot 的群号/QQ 号可能是数字）。
func str(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// onebotInt 把标识转成 OneBot 动作期望的整数（群号/QQ 号）。
func onebotInt(value string) int64 {
	number, _ := strconv.ParseInt(value, 10, 64)
	return number
}
