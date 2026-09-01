// Package contextasm 负责从 Go 管理的会话来源装配中性执行上下文。
//
// 本包只决定本次 Execution 可以看到哪些受治理数据，不决定数据如何被
// 具体 Executor 解释。输出是版本化 JSON payload，而不是
// 具体执行器的消息格式。
package contextasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
)

const (
	SchemaVersion        = "ailuo.context.v1"
	MaxInputPayloadBytes = 16 << 10
)

var (
	ErrInvalidInput          = errors.New("invalid context assembly input")
	ErrContextBudgetExceeded = errors.New("context payload exceeds the assembly budget")
)

// HistorySource 是受限会话历史的最小读取端口，由 session.Service 实现。
type HistorySource interface {
	ListMessages(context.Context, string, string, session.MessageQuery) ([]session.Message, error)
	MessageContent(context.Context, session.Message) ([]byte, error)
}

// Budget 是上下文装配的预算约束。零值字段在 New 时取默认值；MaxAge 为 0
// 表示不限制时间范围。
type Budget struct {
	MaxMessages     int
	MaxCharsPerMsg  int
	MaxTotalChars   int
	MaxAge          time.Duration
	MaxContextBytes int
}

func DefaultBudget() Budget {
	return Budget{
		MaxMessages: 20, MaxCharsPerMsg: 2000, MaxTotalChars: 12000,
		MaxAge: 0, MaxContextBytes: 64 << 10,
	}
}

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
	if budget.MaxContextBytes == 0 {
		budget.MaxContextBytes = defaults.MaxContextBytes
	}
	return budget
}

// HistoryEntry 是一条已装配的历史消息。正文已经按预算裁剪；正文摘要用于
// 固化数据版本，不保存正文本身到日志或来源版本。
type HistoryEntry struct {
	MessageID  string
	Type       string
	Sender     string
	Content    string
	CreatedAt  time.Time
	ContentSHA string
}

type History struct {
	Entries        []HistoryEntry
	Trimmed        int
	TotalChars     int
	TruncatedChars int
	Version        string
}

type SourceVersion struct {
	Version string `json:"version"`
	Count   int    `json:"count,omitempty"`
	Chars   int    `json:"chars,omitempty"`
	Trimmed int    `json:"trimmed,omitempty"`
}

type Sources struct {
	Config       SourceVersion `json:"config"`
	History      SourceVersion `json:"history,omitempty"`
	Capabilities SourceVersion `json:"capabilities"`
}

// ContextBlock 是宿主提供给 Executor 的一个有类型上下文块。Type 只描述
// 数据类别，不指定任何 Executor 的内部表示。
type ContextBlock struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type contextEnvelope struct {
	SchemaVersion string         `json:"schema_version"`
	Blocks        []ContextBlock `json:"blocks"`
}

// Snapshot 是一次 Run 的受控上下文快照。
type Snapshot struct {
	ContextPayload []byte
	Digest         string
	Sources        Sources
	History        History
}

func (s Snapshot) SourcesJSON() json.RawMessage {
	encoded, _ := json.Marshal(s.Sources)
	return encoded
}

// Input 是一次上下文装配输入。输入 payload 独立于上下文传给 Executor；
// SessionID 为空表示本次执行没有会话历史。
type Input struct {
	AppID            string
	SessionID        string
	CurrentMessageID string
	ConfigRevision   string
	Channel          string
	InputContentType string
	InputPayload     []byte
	Capabilities     []string
	Now              time.Time
}

type Assembler struct {
	history HistorySource
	budget  Budget
}

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

func (a *Assembler) Assemble(ctx context.Context, in Input) (Snapshot, error) {
	if err := validateInput(in); err != nil {
		return Snapshot{}, err
	}
	history, err := a.assembleHistory(ctx, in)
	if err != nil {
		return Snapshot{}, err
	}
	payload, err := renderContext(in, history)
	if err != nil {
		return Snapshot{}, err
	}
	for len(payload) > a.budget.MaxContextBytes && len(history.Entries) > 0 {
		history = dropOldest(history)
		payload, err = renderContext(in, history)
		if err != nil {
			return Snapshot{}, err
		}
	}
	if len(payload) > a.budget.MaxContextBytes {
		return Snapshot{}, ErrContextBudgetExceeded
	}
	return Snapshot{
		ContextPayload: payload,
		Digest:         computeDigest(in, history),
		Sources:        buildSources(in, history),
		History:        history,
	}, nil
}

func (a *Assembler) assembleHistory(ctx context.Context, in Input) (History, error) {
	if in.SessionID == "" {
		return History{Version: computeHistoryVersion(nil)}, nil
	}
	messages, err := a.history.ListMessages(ctx, in.AppID, in.SessionID, session.MessageQuery{
		Limit: a.budget.MaxMessages + 1, Descending: true,
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
	slices.Reverse(candidates)
	entries := make([]HistoryEntry, 0, len(candidates))
	truncatedChars := 0
	for _, message := range candidates {
		entry, truncated, included, err := a.buildEntry(ctx, message)
		if err != nil {
			return History{}, err
		}
		if !included {
			continue
		}
		truncatedChars += truncated
		entries = append(entries, entry)
	}
	history := History{Entries: entries, TruncatedChars: truncatedChars}
	for len(entries) > a.budget.MaxMessages {
		entries = entries[1:]
		history.Trimmed++
	}
	for totalChars(entries) > a.budget.MaxTotalChars && len(entries) > 0 {
		entries = entries[1:]
		history.Trimmed++
	}
	history.Entries = entries
	history.TotalChars = totalChars(entries)
	history.Version = computeHistoryVersion(entries)
	return history, nil
}

func (a *Assembler) buildEntry(ctx context.Context, message session.Message) (HistoryEntry, int, bool, error) {
	entry := HistoryEntry{
		MessageID: message.MessageID, Type: message.Type,
		Sender: senderLabel(message.Type), CreatedAt: message.CreatedAt,
	}
	if message.Type != session.MessageTypeText {
		return entry, 0, true, nil
	}
	content, err := a.history.MessageContent(ctx, message)
	if err != nil {
		if errors.Is(err, session.ErrMessageNotFound) {
			return HistoryEntry{}, 0, false, nil
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

func renderContext(in Input, history History) ([]byte, error) {
	blocks := make([]ContextBlock, 0, 2)
	metadata, err := json.Marshal(struct {
		Channel string `json:"channel,omitempty"`
	}{Channel: in.Channel})
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, ContextBlock{Type: "execution.metadata", Payload: metadata})
	if len(history.Entries) > 0 {
		historyPayload, err := json.Marshal(struct {
			Entries []HistoryEntry `json:"entries"`
		}{Entries: history.Entries})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ContextBlock{Type: "conversation.history", Payload: historyPayload})
	}
	return json.Marshal(contextEnvelope{SchemaVersion: SchemaVersion, Blocks: blocks})
}

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

type digestInput struct {
	AppID            string        `json:"app_id"`
	ConfigRevision   string        `json:"config_revision"`
	Channel          string        `json:"channel,omitempty"`
	InputContentType string        `json:"input_content_type"`
	InputSHA         string        `json:"input_sha256"`
	History          []digestEntry `json:"history"`
	Capabilities     []string      `json:"capabilities"`
}

type digestEntry struct {
	MessageID  string `json:"message_id"`
	Type       string `json:"type"`
	ContentSHA string `json:"content_sha256"`
	CreatedAt  string `json:"created_at"`
}

func computeDigest(in Input, history History) string {
	capabilities := append([]string(nil), in.Capabilities...)
	sort.Strings(capabilities)
	inputDigest := sha256.Sum256(in.InputPayload)
	encoded, _ := json.Marshal(digestInput{
		AppID: in.AppID, ConfigRevision: in.ConfigRevision, Channel: in.Channel,
		InputContentType: in.InputContentType, InputSHA: hex.EncodeToString(inputDigest[:]),
		History: digestEntries(history.Entries), Capabilities: capabilities,
	})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func computeHistoryVersion(entries []HistoryEntry) string {
	encoded, _ := json.Marshal(digestEntries(entries))
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func digestEntries(entries []HistoryEntry) []digestEntry {
	result := make([]digestEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, digestEntry{
			MessageID: entry.MessageID, Type: entry.Type, ContentSHA: entry.ContentSHA,
			CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return result
}

func buildSources(in Input, history History) Sources {
	capabilities := append([]string(nil), in.Capabilities...)
	sort.Strings(capabilities)
	return Sources{
		Config:       SourceVersion{Version: in.ConfigRevision, Count: 1},
		History:      SourceVersion{Version: history.Version, Count: len(history.Entries), Chars: history.TotalChars, Trimmed: history.Trimmed},
		Capabilities: SourceVersion{Version: stringListVersion(capabilities), Count: len(capabilities)},
	}
}

func stringListVersion(values []string) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

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

func validateInput(in Input) error {
	switch {
	case !session.ValidStableID(in.AppID):
		return ErrInvalidInput
	case in.SessionID != "" && !session.ValidStableID(in.SessionID):
		return ErrInvalidInput
	case in.CurrentMessageID != "" && !session.ValidStableID(in.CurrentMessageID):
		return ErrInvalidInput
	case strings.TrimSpace(in.ConfigRevision) == "":
		return ErrInvalidInput
	case !validContentType(in.InputContentType):
		return ErrInvalidInput
	case len(in.InputPayload) == 0 || len(in.InputPayload) > MaxInputPayloadBytes:
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

func validContentType(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validateBudget(budget Budget) error {
	if budget.MaxMessages < 1 || budget.MaxMessages > 1000 ||
		budget.MaxCharsPerMsg < 1 || budget.MaxCharsPerMsg > 64<<10 ||
		budget.MaxTotalChars < 1 || budget.MaxTotalChars > 1<<20 ||
		budget.MaxContextBytes < 1 || budget.MaxContextBytes > 64<<10 ||
		(budget.MaxAge != 0 && budget.MaxAge < time.Second) {
		return errors.New("invalid context assembly budget")
	}
	return nil
}
