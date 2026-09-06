// Package qq 提供 QQ 平台适配器（OneBot v11 WebSocket 客户端）：把 QQ 消息
// 事件规范化为标准 InboundMessage 经 Hub 入站，订阅 Echo 事件并把最终回复
// 回发。QQ 白名单与允许来源的身份开通都在本 Access 包完成，未允许来源不会
// 进入 Hub、Message 或 Echo。
package qq

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	defaultDialTimeout    = 10 * time.Second
	defaultReconnectDelay = 5 * time.Second
	defaultRunTimeout     = 3 * time.Minute
	echoWorkerCount       = 4
	echoQueueCapacity     = 32
	maxReplyChunk         = 4000 // QQ 单条消息安全长度上限
)

var (
	cqCodePattern = regexp.MustCompile(`\[CQ:[^\]]*\]`)
	cqAtPattern   = regexp.MustCompile(`\[CQ:at,([^\]]*)\]`)
)

// Config 装配 QQ 适配器。
type Config struct {
	AppID                 string
	WSURL                 string
	Token                 string
	BotQQID               string // 机器人 QQ 号：戳一戳识别 target_id、@提及匹配
	AllowedGroupIDs       []string
	AllowedPrivateUserIDs []string
	QuickReplies          []QuickReply
	PokeReplies           []string
	Provisioner           IdentityProvisioner
	Scheduler             kernelecho.Enqueuer
	OnConnectionChange    func(bool)
	DialTimeout           time.Duration
	ReconnectDelay        time.Duration
	RunTimeout            time.Duration
}

// Adapter 是进程内 QQ 适配器：一条 OneBot 连接，消息事件循环 + 回复回发。
type Adapter struct {
	cfg                 Config
	hub                 *access.Hub
	events              *access.EventHub
	orchestrator        kernelecho.Creator
	scheduler           kernelecho.Enqueuer
	reader              kernelecho.Reader
	allowedGroups       map[string]struct{}
	allowedPrivateUsers map[string]struct{}
	quickReplies        map[string]string
	pokeReplies         []string

	conn   *websocket.Conn
	sendMu sync.Mutex
}

// New 构造 QQ 适配器；配置缺失时返回显式错误。
func New(cfg Config, hub *access.Hub, events *access.EventHub, orchestrator kernelecho.Creator, reader kernelecho.Reader) (*Adapter, error) {
	if cfg.AppID == "" || cfg.WSURL == "" || cfg.Provisioner == nil || cfg.Scheduler == nil || hub == nil || events == nil || orchestrator == nil || reader == nil {
		return nil, errors.New("qq adapter configuration is incomplete")
	}
	normalized, err := qqsettings.Normalize(qqsettings.Settings{
		AppID: cfg.AppID, Enabled: true, WSURL: cfg.WSURL, BotQQID: cfg.BotQQID,
		AllowedGroupIDs: cfg.AllowedGroupIDs, AllowedPrivateUserIDs: cfg.AllowedPrivateUserIDs,
	})
	if err != nil {
		return nil, errors.New("qq adapter bot qq id is invalid")
	}
	cfg.AppID, cfg.WSURL, cfg.BotQQID = normalized.AppID, normalized.WSURL, normalized.BotQQID
	cfg.AllowedGroupIDs = normalized.AllowedGroupIDs
	cfg.AllowedPrivateUserIDs = normalized.AllowedPrivateUserIDs
	cfg.QuickReplies, cfg.PokeReplies, err = NormalizeBehavior(cfg.QuickReplies, cfg.PokeReplies)
	if err != nil {
		return nil, err
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
		cfg: cfg, hub: hub, events: events, orchestrator: orchestrator, scheduler: cfg.Scheduler, reader: reader,
		allowedGroups: toSet(cfg.AllowedGroupIDs), allowedPrivateUsers: toSet(cfg.AllowedPrivateUserIDs),
		quickReplies: toQuickReplyMap(cfg.QuickReplies), pokeReplies: append([]string(nil), cfg.PokeReplies...),
	}, nil
}

// Run 连接 OneBot WebSocket 并处理消息；断线按 ReconnectDelay 重连，
// ctx 取消时退出。
func (a *Adapter) Run(ctx context.Context) error {
	echoEvents := make(chan map[string]any, echoQueueCapacity)
	var workerWG sync.WaitGroup
	workerWG.Add(echoWorkerCount)
	for range echoWorkerCount {
		go func() {
			defer workerWG.Done()
			a.runEchoWorker(ctx, echoEvents)
		}()
	}
	defer func() {
		close(echoEvents)
		workerWG.Wait()
	}()
	for {
		err := a.serve(ctx, echoEvents)
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

func (a *Adapter) runEchoWorker(ctx context.Context, events <-chan map[string]any) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			a.handleEvent(ctx, event)
		}
	}
}

func (a *Adapter) serve(ctx context.Context, echoEvents chan<- map[string]any) error {
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
	a.sendMu.Lock()
	a.conn = conn
	a.sendMu.Unlock()
	if a.cfg.OnConnectionChange != nil {
		a.cfg.OnConnectionChange(true)
	}
	connectionDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-connectionDone:
		}
	}()
	defer func() {
		close(connectionDone)
		a.sendMu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.sendMu.Unlock()
		_ = conn.Close()
		if a.cfg.OnConnectionChange != nil {
			a.cfg.OnConnectionChange(false)
		}
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
		select {
		case echoEvents <- event:
		case <-ctx.Done():
			return nil
		default:
			observe.Warn(ctx, "QQ Echo 处理队列已满，拒绝新消息",
				observe.StringAttr("platform_message_id", str(event, "message_id")),
			)
		}
	}
}

// handleEvent 处理一条 OneBot 消息事件：标准入站 → 幂等 Echo 创建 → 等待
// 终态回复 → 回发。未绑定身份等入站失败回发公共错误，不泄露内部细节。
func (a *Adapter) handleEvent(ctx context.Context, raw map[string]any) {
	inbound, mentioned := normalizeEvent(a.cfg.AppID, a.cfg.BotQQID, raw)
	if inbound == nil {
		return
	}
	if !a.allowed(inbound) {
		return
	}
	if reply, matched := a.quickReplies[inbound.Text]; matched {
		a.replyPlain(ctx, inbound, reply)
		return
	}
	if inbound.PlatformChannel == "group" && !mentioned {
		return // 普通群消息默认只响应 @提及，快速回复已在此前直接处理
	}
	if err := a.cfg.Provisioner.EnsureQQIdentity(ctx, *inbound); err != nil {
		_, _, message := access.IntakePublicError(err)
		a.reply(ctx, inbound, message)
		return
	}
	intake, err := a.hub.Intake(ctx, *inbound)
	if err != nil {
		_, code, message := access.IntakePublicError(err)
		observe.Warn(ctx, "QQ 标准消息接入被拒绝",
			observe.StringAttr("error_code", code),
			observe.StringAttr("platform_message_id", inbound.PlatformMessageID),
		)
		a.reply(ctx, inbound, message)
		return
	}
	echoID, created, err := a.orchestrator.CreateIdempotent(ctx, kernelecho.RunRequest{
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
	if created {
		a.scheduler.Enqueue(ctx, echoID)
	}
	a.forwardReplies(ctx, inbound, echoID)
}

// forwardReplies 按持久事件序号转发 root 回复和子 Agent 生命周期通知。
// root 回复保留群聊 @，子 Agent 的创建与终态通知始终使用纯文本发送。
func (a *Adapter) forwardReplies(ctx context.Context, inbound *access.InboundMessage, echoID string) {
	live, unsubscribe := a.events.Subscribe(a.cfg.AppID, echoID)
	defer unsubscribe()
	lifecycle := newSubagentReplyLifecycle()
	lastSequence := uint64(0)
	process := func(event kernelecho.Event) {
		if event.Sequence <= lastSequence {
			return
		}
		lastSequence = event.Sequence
		switch event.Type {
		case "subagent.created":
			if !lifecycle.observe(event) {
				a.logInvalidSubagentLifecycle(ctx, echoID, event)
			}
			a.replyPlain(ctx, inbound, "已派出子 Agent")
		case "subagent.completed":
			if !lifecycle.observe(event) {
				a.logInvalidSubagentLifecycle(ctx, echoID, event)
			}
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && strings.TrimSpace(payload.Text) != "" {
				a.replyPlain(ctx, inbound, "子 Agent 已完成："+payload.Text)
			} else {
				a.replyPlain(ctx, inbound, "子 Agent 已完成")
			}
		case "subagent.failed":
			if !lifecycle.observe(event) {
				a.logInvalidSubagentLifecycle(ctx, echoID, event)
			}
			var payload struct {
				Message string `json:"message"`
			}
			message := "子 Agent 执行失败"
			if json.Unmarshal(event.Payload, &payload) == nil && strings.TrimSpace(payload.Message) != "" {
				message = payload.Message
			}
			a.replyPlain(ctx, inbound, message)
		case "subagent.cancelled":
			if !lifecycle.observe(event) {
				a.logInvalidSubagentLifecycle(ctx, echoID, event)
			}
			a.replyPlain(ctx, inbound, "子 Agent 已取消")
		default:
			if reply := terminalReply(event); reply != nil {
				lifecycle.rootTerminal = true
				a.reply(ctx, inbound, *reply)
			}
		}
	}
	record, persisted, err := a.reader.GetEcho(ctx, a.cfg.AppID, echoID)
	if err == nil {
		for _, event := range persisted {
			process(event)
		}
		if record.Status != kernelecho.StatusRunning || lifecycle.complete() {
			return
		}
	} else {
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
			return
		case <-timer.C:
			observe.Warn(ctx, "等待 QQ Echo 事件超时", observe.StringAttr("echo_id", echoID))
			return
		case event, open := <-live:
			if !open {
				return
			}
			process(event)
			if lifecycle.complete() {
				return
			}
		}
	}
}

type subagentReplyLifecycle struct {
	pendingChildren   map[string]struct{}
	terminalChildren  map[string]struct{}
	untrackedChildren int
	rootTerminal      bool
}

func newSubagentReplyLifecycle() *subagentReplyLifecycle {
	return &subagentReplyLifecycle{
		pendingChildren:  make(map[string]struct{}),
		terminalChildren: make(map[string]struct{}),
	}
}

func (l *subagentReplyLifecycle) observe(event kernelecho.Event) bool {
	var payload struct {
		RunID string `json:"run_id"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || strings.TrimSpace(payload.RunID) == "" {
		if event.Type == "subagent.created" {
			l.untrackedChildren++
		}
		return false
	}
	if event.Type == "subagent.created" {
		if _, terminal := l.terminalChildren[payload.RunID]; !terminal {
			l.pendingChildren[payload.RunID] = struct{}{}
		}
		return true
	}
	l.terminalChildren[payload.RunID] = struct{}{}
	delete(l.pendingChildren, payload.RunID)
	return true
}

func (l *subagentReplyLifecycle) complete() bool {
	return l.rootTerminal && len(l.pendingChildren) == 0 && l.untrackedChildren == 0
}

func (a *Adapter) logInvalidSubagentLifecycle(ctx context.Context, echoID string, event kernelecho.Event) {
	observe.Warn(ctx, "QQ 子 Agent 生命周期事件缺少有效 run_id",
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("event_type", event.Type),
		observe.Int64Attr("event_sequence", int64(event.Sequence)),
	)
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

// reply 把回复文本发送到消息来源。群消息使用 OneBot 消息段明确 @ 原发送人，
// 私聊直接回用户；按 PlatformChannel 选择动作，不依赖空间 ID 是否为空。
func (a *Adapter) reply(ctx context.Context, inbound *access.InboundMessage, text string) {
	a.sendReply(ctx, inbound, text, true)
}

// replyPlain 发送不带 @ 的平台固定回复，用于快速回复与戳一戳。
func (a *Adapter) replyPlain(ctx context.Context, inbound *access.InboundMessage, text string) {
	a.sendReply(ctx, inbound, text, false)
}

func (a *Adapter) sendReply(ctx context.Context, inbound *access.InboundMessage, text string, mentionSender bool) {
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
			if mentionSender {
				params["message"] = []any{
					map[string]any{"type": "at", "data": map[string]any{"qq": inbound.PlatformUserID}},
					map[string]any{"type": "text", "data": map[string]any{"text": " " + chunk}},
				}
			}
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

func (a *Adapter) allowed(inbound *access.InboundMessage) bool {
	if inbound == nil {
		return false
	}
	if inbound.PlatformChannel == "group" {
		_, allowed := a.allowedGroups[inbound.PlatformSpaceID]
		return allowed
	}
	if inbound.PlatformChannel == "private" {
		_, allowed := a.allowedPrivateUsers[inbound.PlatformUserID]
		return allowed
	}
	return false
}

func toSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func toQuickReplyMap(values []QuickReply) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Trigger] = value.Reply
	}
	return result
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
func normalizeEvent(appID, botQQID string, raw map[string]any) (*access.InboundMessage, bool) {
	if str(raw, "post_type") != "message" {
		return nil, false
	}
	if selfID := str(raw, "self_id"); selfID != "" && selfID != botQQID {
		return nil, false
	}

	userID := str(raw, "user_id")
	rawMessageID := str(raw, "message_id")
	if userID == "" || rawMessageID == "" || userID == botQQID {
		return nil, false
	}
	messageID := stablePlatformMessageID(rawMessageID)
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
	text, mentioned := extractText(raw, botQQID)
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

// stablePlatformMessageID 保留已经满足内核稳定标识约束的平台消息号；LLBot
// 等实现可能返回负整数或其他不透明值，此时使用确定性摘要承载幂等语义。
func stablePlatformMessageID(value string) string {
	if session.ValidStableID(value) && session.ValidPlatformMessageID(value) {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("qq-message-v1-%x", digest[:])
}

// extractText 从消息事件提取纯文本：优先拼接 array 模式的 text 段，退化时
// 从 raw_message 剥离 CQ 码。同时返回是否包含 at 段（任意用户）。
func extractText(raw map[string]any, botQQID string) (text string, mentioned bool) {
	if segments, ok := raw["message"].([]any); ok {
		var builder strings.Builder
		for _, segment := range segments {
			seg, ok := segment.(map[string]any)
			if !ok {
				continue
			}
			data, _ := seg["data"].(map[string]any)
			switch str(seg, "type") {
			case "text":
				builder.WriteString(str(data, "text"))
			case "at":
				if str(data, "qq") == botQQID {
					mentioned = true
				}
			}
		}
		if text := strings.TrimSpace(builder.String()); text != "" {
			return text, mentioned
		}
	}
	rawMessage := str(raw, "raw_message")
	return strings.TrimSpace(cqCodePattern.ReplaceAllString(rawMessage, "")), mentioned || rawMentionsBot(rawMessage, botQQID)
}

func rawMentionsBot(rawMessage, botQQID string) bool {
	for _, match := range cqAtPattern.FindAllStringSubmatch(rawMessage, -1) {
		for _, parameter := range strings.Split(match[1], ",") {
			key, value, found := strings.Cut(parameter, "=")
			if found && key == "qq" && value == botQQID {
				return true
			}
		}
	}
	return false
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
