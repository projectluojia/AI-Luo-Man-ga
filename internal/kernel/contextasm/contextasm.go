// Package contextasm 是「上下文与记忆装配」模块的内核侧实现。
//
// 职责：由 Go 决定模型本次能够看到什么，Python 只接收装配完成的系统提示，
// 不持有会话数据库凭据。装配器按 Run 从受治理来源构造确定性上下文快照：
//
//	系统提示配置修订
//	+ 当前标准消息
//	+ 受限会话历史（条目数 / 字符数 / 时间范围预算）
//	+ 当前 Capability 投影
//
// 快照摘要（Digest）是配置修订、当前消息、历史数据版本与 Capability 投影的
// 确定性哈希：相同配置修订与数据版本必然产生相同摘要，墙钟时间不进入摘要。
// 各来源版本（Sources）被固化到 Run 记录，供恢复与审计；消息正文永不进入
// 日志、审计载荷或公共 API——只记录条数、字符数与裁剪数。
//
// 本包只装配当前已存在且受治理的来源。知识库证据与授权长期记忆分属独立
// 模块（本包不为其预留空壳端口）；相关来源落地后按同一契约扩展装配输入。
package contextasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
)

var (
	// ErrInvalidInput 装配输入不合法。
	ErrInvalidInput = errors.New("invalid context assembly input")
	// ErrPromptBudgetExceeded 基础系统提示单独装配即超出提示总预算（配置或预算错误）。
	ErrPromptBudgetExceeded = errors.New("base system prompt exceeds the context prompt budget")
)

// HistorySource 是受限会话历史的最小读取端口，由 session.Service 实现。
// ListMessages 按 App 与 Session 约束返回未删除消息；MessageContent 按消息的
// ContentRef 装配正文（内联与 Blob 模式统一处理）。
type HistorySource interface {
	ListMessages(context.Context, string, string, session.MessageQuery) ([]session.Message, error)
	MessageContent(context.Context, session.Message) ([]byte, error)
}

// Budget 是上下文装配的预算约束。零值字段在 New 时取默认值；MaxAge 为 0
// 表示不限制时间范围。
type Budget struct {
	MaxMessages    int           // 会话历史最大条目数
	MaxCharsPerMsg int           // 单条历史消息最大字符数
	MaxTotalChars  int           // 会话历史总字符数上限
	MaxAge         time.Duration // 时间范围预算；0 表示不限制
	MaxPromptBytes int           // 装配后系统提示总字节上限（不超过协议上限）
}

// DefaultBudget 返回上下文装配的默认预算。
func DefaultBudget() Budget {
	return Budget{
		MaxMessages:    20,
		MaxCharsPerMsg: 2000,
		MaxTotalChars:  12000,
		MaxAge:         0,
		MaxPromptBytes: 24 << 10,
	}
}

// normalizeBudget 把零值预算字段替换为默认值。MaxAge 为 0 保持"不限制"语义。
func normalizeBudget(budget Budget) Budget {
	defaults := DefaultBudget()
	if budget.MaxMessages == 0 {
		budget.MaxMessages = defaults.MaxMessages
	}
	if budget.MaxCharsPerMsg == 0 {
		budget.MaxCharsPerMsg = defaults.MaxCharsPerMsg
	}
	if budget.MaxTotalChars == 0 {
		budget.MaxTotalChars = defaults.MaxTotalChars
	}
	if budget.MaxPromptBytes == 0 {
		budget.MaxPromptBytes = defaults.MaxPromptBytes
	}
	return budget
}

// HistoryEntry 是一条已装配的历史消息。正文已经被预算裁剪；正文摘要用于
// 固化数据版本，不保存正文本身。
type HistoryEntry struct {
	MessageID  string    // 稳定消息标识
	Type       string    // 消息类型（text/image/file/system/event）
	Sender     string    // 发送者标签（用户/图片消息/文件消息/系统消息）
	Content    string    // 预算裁剪后的正文（非文本消息为空）
	CreatedAt  time.Time // 消息时间
	ContentSHA string    // 正文 SHA-256（hex），覆盖完整正文而非裁剪后正文
}

// History 是受限会话历史的装配结果。Entries 按时间升序（最旧在前）。
type History struct {
	Entries        []HistoryEntry
	Trimmed        int // 因条目/字符/提示总预算被丢弃的条数
	TotalChars     int // 实际纳入的历史总字符数
	TruncatedChars int // 因单条字符预算被截断的字符数
	Version        string
}

// SourceVersion 是一个上下文来源的固化版本（审计与恢复契约，不含正文）。
type SourceVersion struct {
	Version string `json:"version"`
	Count   int    `json:"count,omitempty"`
	Chars   int    `json:"chars,omitempty"`
	Trimmed int    `json:"trimmed,omitempty"`
}

// Sources 是 Run 上下文的来源版本集合。正文永不进入本结构。
type Sources struct {
	Config       SourceVersion `json:"config"`
	History      SourceVersion `json:"history,omitempty"`
	Capabilities SourceVersion `json:"capabilities"`
}

// Snapshot 是一次 Run 的受控上下文快照。
type Snapshot struct {
	SystemPrompt string  // 渲染后的完整系统提示（基础提示 + 当前时间 + 历史块）
	Digest       string  // 确定性快照摘要（sha256 hex）
	Sources      Sources // 固化来源版本
	History      History // 受限会话历史（含统计）
}

// SourcesJSON 返回固化来源版本的规范 JSON（写入 Run 记录，正文绝不进入）。
func (s Snapshot) SourcesJSON() json.RawMessage {
	encoded, _ := json.Marshal(s.Sources)
	return encoded
}

// Input 是一次上下文装配的输入。SessionID 为空表示无会话（如子 Run），
// 装配器跳过历史装配；此时快照仍覆盖配置、当前消息与 Capability 投影。
type Input struct {
	AppID            string
	SessionID        string   // 空表示无会话，跳过历史装配
	CurrentMessageID string   // 当前标准消息标识；从历史中排除
	ConfigRevision   string   // 系统提示配置修订
	SystemPrompt     string   // 基础系统提示（配置系统提示 + Run 策略后缀）
	Timezone         string   // IANA 时区
	Capabilities     []string // 当前投影的 Capability（"id@version"）
	InputMessage     string   // 当前标准消息正文
	Now              time.Time
}

// Assembler 是上下文装配器：按 Run 构造确定性上下文快照并执行预算。
type Assembler struct {
	history HistorySource
	budget  Budget
}

// New 构造装配器。history 是会话历史读取端口，不可为空（装配失败即显式错误）；
// budget 的零值字段取默认值，非法预算属于装配期编程错误。
func New(history HistorySource, budget Budget) (*Assembler, error) {
	if history == nil {
		return nil, errors.New("context assembler requires a session history source")
	}
	budget = normalizeBudget(budget)
	if err := validateBudget(budget); err != nil {
		return nil, err
	}
	return &Assembler{history: history, budget: budget}, nil
}

// Assemble 按输入构造上下文快照。返回的快照是确定性的：相同配置修订与
// 数据版本必然产生相同 Digest；墙钟时间只影响渲染，不进入摘要。
func (a *Assembler) Assemble(ctx context.Context, in Input) (Snapshot, error) {
	if err := validateInput(in); err != nil {
		return Snapshot{}, err
	}
	location, err := time.LoadLocation(in.Timezone)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: invalid timezone", ErrInvalidInput)
	}
	history, err := a.assembleHistory(ctx, in)
	if err != nil {
		return Snapshot{}, err
	}
	prompt := renderPrompt(in, location, history)
	for len(prompt) > a.budget.MaxPromptBytes && len(history.Entries) > 0 {
		history = dropOldest(history)
		prompt = renderPrompt(in, location, history)
	}
	if len(prompt) > a.budget.MaxPromptBytes {
		return Snapshot{}, ErrPromptBudgetExceeded
	}
	return Snapshot{
		SystemPrompt: prompt,
		Digest:       computeDigest(in, history),
		Sources:      buildSources(in, history),
		History:      history,
	}, nil
}

// assembleHistory 读取受限会话历史并执行条目数、字符数与时间范围预算。
// 裁剪策略固定：优先保留最新、丢弃最旧；超龄消息直接排除。
func (a *Assembler) assembleHistory(ctx context.Context, in Input) (History, error) {
	if in.SessionID == "" {
		return History{Version: computeHistoryVersion(nil)}, nil
	}
	messages, err := a.history.ListMessages(ctx, in.AppID, in.SessionID, session.MessageQuery{
		Limit:      a.budget.MaxMessages + 1, // 多取一条，用于排除当前消息
		Descending: true,
	})
	if err != nil {
		return History{}, err
	}
	candidates := make([]session.Message, 0, len(messages))
	for _, message := range messages {
		if message.MessageID == in.CurrentMessageID {
			continue
		}
		if a.budget.MaxAge > 0 && in.Now.Sub(message.CreatedAt) > a.budget.MaxAge {
			continue
		}
		candidates = append(candidates, message)
	}
	slices.Reverse(candidates) // 倒序查询 → 按时间升序装配（最旧在前）
	entries := make([]HistoryEntry, 0, len(candidates))
	truncatedChars := 0
	for _, message := range candidates {
		entry, truncated, included, err := a.buildEntry(ctx, message)
		if err != nil {
			return History{}, err
		}
		if !included {
			continue // 列表读取与正文读取之间被并发删除
		}
		truncatedChars += truncated
		entries = append(entries, entry)
	}
	history := History{Entries: entries, TruncatedChars: truncatedChars}
	// 条目数预算：超过 MaxMessages 时从最旧开始丢弃（查询多取一条用于排除
	// 当前消息，无当前消息时该条目也按预算裁剪）。
	for len(entries) > a.budget.MaxMessages {
		entries = entries[1:]
		history.Trimmed++
	}
	// 总字符预算：仍超出时从最旧开始丢弃。
	for totalChars(entries) > a.budget.MaxTotalChars && len(entries) > 0 {
		entries = entries[1:] // 丢弃最旧
		history.Trimmed++
	}
	history.Entries = entries
	history.TotalChars = totalChars(entries)
	history.Version = computeHistoryVersion(entries)
	return history, nil
}

// buildEntry 装配单条历史消息。非文本消息不读取正文，使用类型占位标签；
// 文本消息读取完整正文并截断到单条字符预算。正文摘要覆盖完整正文。
func (a *Assembler) buildEntry(ctx context.Context, message session.Message) (HistoryEntry, int, bool, error) {
	entry := HistoryEntry{
		MessageID: message.MessageID,
		Type:      message.Type,
		Sender:    senderLabel(message.Type),
		CreatedAt: message.CreatedAt,
	}
	if message.Type != session.MessageTypeText {
		return entry, 0, true, nil
	}
	content, err := a.history.MessageContent(ctx, message)
	if err != nil {
		if errors.Is(err, session.ErrMessageNotFound) {
			return HistoryEntry{}, 0, false, nil // 并发删除：跳过
		}
		return HistoryEntry{}, 0, false, err
	}
	digest := sha256.Sum256(content)
	entry.ContentSHA = hex.EncodeToString(digest[:])
	runes := []rune(string(content))
	truncated := 0
	if len(runes) > a.budget.MaxCharsPerMsg {
		truncated = len(runes) - a.budget.MaxCharsPerMsg
		runes = runes[:a.budget.MaxCharsPerMsg]
	}
	entry.Content = string(runes)
	return entry, truncated, true, nil
}

// renderPrompt 渲染完整系统提示：基础提示 + 当前系统时间 + 历史块。
// 时间按输入时区转换后格式化；历史块只包含预算后的条目。
func renderPrompt(in Input, location *time.Location, history History) string {
	prompt := in.SystemPrompt
	prompt += "\n当前系统时间：" + in.Now.In(location).Format("2006-01-02 15:04:05 -0700") + "（" + in.Timezone + "）。"
	if len(history.Entries) == 0 {
		return prompt
	}
	prompt += "\n\n【历史对话】"
	for _, entry := range history.Entries {
		timestamp := entry.CreatedAt.In(location).Format("2006-01-02 15:04")
		if entry.Content == "" {
			prompt += "\n" + entry.Sender + "（" + timestamp + "）"
		} else {
			prompt += "\n" + entry.Sender + "（" + timestamp + "）：" + entry.Content
		}
	}
	return prompt
}

// senderLabel 按消息类型给出历史条目的发送者标签。当前消息模型只持久化
// 平台标准消息（用户侧），因此文本/图片/文件均为用户消息；系统与事件消息
// 使用独立标签，不伪装成用户发言。
func senderLabel(messageType string) string {
	switch messageType {
	case session.MessageTypeImage:
		return "图片消息"
	case session.MessageTypeFile:
		return "文件消息"
	case session.MessageTypeSystem, session.MessageTypeEvent:
		return "系统消息"
	default:
		return "用户"
	}
}

// digestInput 是快照摘要的规范输入：相同配置修订与数据版本必然产生相同摘要。
// 墙钟时间不进入摘要（它是运行期环境而非数据版本）。
type digestInput struct {
	AppID          string        `json:"app_id"`
	ConfigRevision string        `json:"config_revision"`
	SystemPrompt   string        `json:"system_prompt"`
	Timezone       string        `json:"timezone"`
	InputMessage   string        `json:"input_message"`
	History        []digestEntry `json:"history"`
	Capabilities   []string      `json:"capabilities"`
}

// digestEntry 是历史消息在摘要中的身份：消息标识、类型、正文摘要与时间。
type digestEntry struct {
	MessageID  string `json:"message_id"`
	Type       string `json:"type"`
	ContentSHA string `json:"content_sha256"`
	CreatedAt  string `json:"created_at"`
}

// computeDigest 计算确定性快照摘要（sha256 hex）。
func computeDigest(in Input, history History) string {
	capabilities := append([]string(nil), in.Capabilities...)
	sort.Strings(capabilities)
	encoded, _ := json.Marshal(digestInput{
		AppID:          in.AppID,
		ConfigRevision: in.ConfigRevision,
		SystemPrompt:   in.SystemPrompt,
		Timezone:       in.Timezone,
		InputMessage:   in.InputMessage,
		History:        digestEntries(history.Entries),
		Capabilities:   capabilities,
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// computeHistoryVersion 计算历史数据版本：纳入历史消息身份与正文摘要的
// 确定性哈希；空历史也有确定的版本值。
func computeHistoryVersion(entries []HistoryEntry) string {
	encoded, _ := json.Marshal(digestEntries(entries))
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func digestEntries(entries []HistoryEntry) []digestEntry {
	result := make([]digestEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, digestEntry{
			MessageID:  entry.MessageID,
			Type:       entry.Type,
			ContentSHA: entry.ContentSHA,
			CreatedAt:  entry.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return result
}

// buildSources 固化各来源版本：配置修订、历史数据版本与 Capability 投影版本。
func buildSources(in Input, history History) Sources {
	capabilities := append([]string(nil), in.Capabilities...)
	sort.Strings(capabilities)
	return Sources{
		Config: SourceVersion{Version: in.ConfigRevision, Count: 1},
		History: SourceVersion{
			Version: history.Version,
			Count:   len(history.Entries),
			Chars:   history.TotalChars,
			Trimmed: history.Trimmed,
		},
		Capabilities: SourceVersion{Version: stringListVersion(capabilities), Count: len(capabilities)},
	}
}

func stringListVersion(values []string) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// dropOldest 丢弃最旧的历史条目并更新统计（预算裁剪）。
func dropOldest(history History) History {
	if len(history.Entries) == 0 {
		return history
	}
	dropped := history.Entries[0]
	history.Entries = history.Entries[1:]
	history.Trimmed++
	history.TotalChars -= utf8.RuneCountInString(dropped.Content)
	history.Version = computeHistoryVersion(history.Entries)
	return history
}

func totalChars(entries []HistoryEntry) int {
	total := 0
	for _, entry := range entries {
		total += utf8.RuneCountInString(entry.Content)
	}
	return total
}

// validateInput 校验装配输入。所有标识必须是合法稳定标识；正文必须是合法
// UTF-8 且非空；时区必须可加载。
func validateInput(in Input) error {
	switch {
	case !session.ValidStableID(in.AppID):
		return ErrInvalidInput
	case in.SessionID != "" && !session.ValidStableID(in.SessionID):
		return ErrInvalidInput
	case in.CurrentMessageID != "" && !session.ValidStableID(in.CurrentMessageID):
		return ErrInvalidInput
	case in.ConfigRevision == "":
		return ErrInvalidInput
	case strings.TrimSpace(in.SystemPrompt) == "" || !utf8.ValidString(in.SystemPrompt):
		return ErrInvalidInput
	case strings.TrimSpace(in.InputMessage) == "" || !utf8.ValidString(in.InputMessage):
		return ErrInvalidInput
	case in.Timezone == "":
		return ErrInvalidInput
	case in.Now.IsZero():
		return ErrInvalidInput
	}
	for _, capability := range in.Capabilities {
		if capability == "" || len(capability) > 256 || !utf8.ValidString(capability) {
			return ErrInvalidInput
		}
	}
	return nil
}

// Budget 校验：装配器的预算必须位于协议允许范围内（提示总大小不超过
// Agent 协议的系统提示上限）。New 时校验，非法预算属于装配期编程错误。
func validateBudget(budget Budget) error {
	if budget.MaxMessages < 1 || budget.MaxMessages > 1000 ||
		budget.MaxCharsPerMsg < 1 || budget.MaxCharsPerMsg > 64<<10 ||
		budget.MaxTotalChars < 1 || budget.MaxTotalChars > 1<<20 ||
		budget.MaxPromptBytes < 1 || budget.MaxPromptBytes > agentprotocol.MaxSystemPromptBytes ||
		(budget.MaxAge != 0 && budget.MaxAge < time.Second) {
		return errors.New("invalid context assembly budget")
	}
	return nil
}
