package kiro

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	thinkingStartTag = "<thinking>"
	thinkingEndTag   = "</thinking>"

	// maxPendingWhitespace 是等待判断是否出现 <thinking> 时最多缓存的前导空白。
	maxPendingWhitespace = 1024
)

// ErrEmptyResponse 表示上游没有生成任何内容。此时尚未向客户端写出数据，
// 调用方可以换账号重试。
var ErrEmptyResponse = errors.New("kiro returned an empty response")

// SSEEvent 是一个 Anthropic Messages 流事件。
type SSEEvent struct {
	Event string
	Data  []byte
}

// AppendTo 把事件按 SSE 格式追加到 dst。
func (e SSEEvent) AppendTo(dst []byte) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, e.Event...)
	dst = append(dst, "\ndata: "...)
	dst = append(dst, e.Data...)
	return append(dst, "\n\n"...)
}

// Usage 是本次响应的 token 用量。Kiro 不返回精确用量：输出按文本估算，
// 输入由 contextUsagePercentage 反推，缺失时使用调用方给出的估算值；缓存
// 读写按 CachePlan 从输入中拆出。
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	CacheCreation5mTokens    int
	CacheCreation1hTokens    int
}

// ConverterOptions 配置 Converter。
type ConverterOptions struct {
	// Model 是返回给客户端的模型名。
	Model string
	// UpstreamModel 用于确定上下文窗口。
	UpstreamModel string
	// MessageID 为空时自动生成。
	MessageID string
	// ThinkingRequested 为 true 时解析正文里的 <thinking> 标签。
	ThinkingRequested bool
	// Tools 用于还原被缩短的工具名。
	Tools *ToolNames
	// EstimatedInputTokens 在上游未报告上下文占用时作为输入 token 数。
	EstimatedInputTokens int
	// CountTokens 估算文本 token 数，为空时按字符数粗略估算。
	CountTokens func(string) int
	// CachePlan 为空时输入全部按未缓存计算。
	CachePlan *CachePlan
}

type blockKind string

const (
	blockText     blockKind = "text"
	blockThinking blockKind = "thinking"
	blockToolUse  blockKind = "tool_use"
)

type contentBlock struct {
	kind  blockKind
	text  strings.Builder
	id    string
	name  string
	input strings.Builder
}

// Converter 把 Kiro 流事件转换为 Anthropic Messages SSE 事件，并同时累积
// 非流式响应所需的内容。
//
// message_start 会延迟到第一个内容事件才输出：上游空响应时客户端没有收到
// 任何数据，调用方可以安全地换账号重试。
type Converter struct {
	opts ConverterOptions

	started  bool
	finished bool
	blocks   []*contentBlock
	// open 是当前打开的块，同一时刻最多一个。
	open *contentBlock

	// <thinking> 标签解析状态。
	buf                   string
	pendingBeforeThinking string
	inThinking            bool
	thinkingExtracted     bool
	stripThinkingNewline  bool
	stripTextNewlines     bool

	lastText       string
	hasVisibleText bool
	hasThinking    bool
	outputText     strings.Builder

	ignoredTools map[string]struct{}
	toolCount    int

	contextUsagePercentage float64
	stopReason             string
	usage                  Usage
}

// NewConverter 创建 Converter。
func NewConverter(opts ConverterOptions) *Converter {
	if opts.MessageID == "" {
		opts.MessageID = "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if opts.CountTokens == nil {
		opts.CountTokens = roughTokenCount
	}
	return &Converter{opts: opts, ignoredTools: map[string]struct{}{}}
}

// Started 报告是否已经输出过 message_start，即客户端是否已经收到数据。
func (c *Converter) Started() bool { return c.started }

// Usage 返回用量，Finish 或 SettleUsage 之后有效。
func (c *Converter) Usage() Usage { return c.usage }

// SettleUsage 在流被中断、没有正常 Finish 时按已收到的内容计算用量。客户端
// 还没收到任何数据时用量保持为零。
func (c *Converter) SettleUsage() {
	if c.finished || !c.started {
		return
	}
	c.usage = c.computeUsage()
}

// minReplayedChunkLen 是按重复片段丢弃的最短长度。
const minReplayedChunkLen = 16

// isReplayedChunk 判断正文片段是否是 Kiro 重复下发的上一段。
// Kiro 偶尔连续发送两次同一段正文；但模型自己也常连续输出相同的短片段，
// 例如缩进、表格分隔符、换行，这些必须保留。所以只把足够长、且不全是
// 空白的完全相同片段当作重复。
func (c *Converter) isReplayedChunk(text string) bool {
	return text == c.lastText &&
		len(text) >= minReplayedChunkLen &&
		strings.TrimSpace(text) != ""
}

// StopReason 返回结束原因，Finish 之后有效。
func (c *Converter) StopReason() string { return c.stopReason }

// Handle 处理一个上游事件，返回需要发给客户端的 SSE 事件。
func (c *Converter) Handle(ev Event) []SSEEvent {
	var out []SSEEvent
	switch ev.Kind {
	case EventText:
		if ev.Text == "" || c.isReplayedChunk(ev.Text) {
			return nil
		}
		c.lastText = ev.Text
		if !c.opts.ThinkingRequested {
			c.appendText(blockText, ev.Text, &out)
			break
		}
		c.buf += ev.Text
		c.processThinkingTags(&out)
	case EventReasoning:
		c.appendText(blockThinking, ev.Text, &out)
	case EventToolUse:
		c.handleToolUse(ev, &out)
	case EventContextUsage:
		if ev.ContextUsagePercentage > 0 {
			c.contextUsagePercentage = ev.ContextUsagePercentage
		}
	}
	return c.withMessageStart(out)
}

// Finish 结束转换并返回剩余事件。上游没有生成任何内容时返回
// ErrEmptyResponse，且不返回事件。
func (c *Converter) Finish() ([]SSEEvent, error) {
	if c.finished {
		return nil, nil
	}
	var out []SSEEvent
	c.closeTool(&out)
	c.flushThinkingTags(&out)
	if c.open != nil && c.open.kind == blockThinking {
		c.closeBlock(&out)
	}

	thinkingOnly := c.hasThinking && !c.hasVisibleText && c.toolCount == 0
	if !thinkingOnly && c.outputText.Len() == 0 && c.toolCount == 0 {
		return nil, ErrEmptyResponse
	}
	if thinkingOnly {
		// 只有思考没有正文时补一个最小正文块，客户端按 max_tokens 处理。
		c.appendText(blockText, " ", &out)
	}
	c.closeBlock(&out)
	c.finished = true

	switch {
	case c.toolCount > 0:
		c.stopReason = "tool_use"
	case thinkingOnly:
		c.stopReason = "max_tokens"
	default:
		c.stopReason = "end_turn"
	}
	c.usage = c.computeUsage()
	out = append(out,
		newSSE("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": c.stopReason, "stop_sequence": nil},
			"usage": c.usageMap(),
		}),
		newSSE("message_stop", map[string]any{"type": "message_stop"}),
	)
	return c.withMessageStart(out), nil
}

// Message 返回非流式响应体，Finish 成功之后调用。
func (c *Converter) Message() []byte {
	content := make([]map[string]any, 0, len(c.blocks))
	for _, b := range c.blocks {
		switch b.kind {
		case blockText:
			content = append(content, map[string]any{"type": "text", "text": b.text.String()})
		case blockThinking:
			content = append(content, map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": ""})
		case blockToolUse:
			content = append(content, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": toolInputObject(b.input.String())})
		}
	}
	data, _ := json.Marshal(map[string]any{
		"id":            c.opts.MessageID,
		"type":          "message",
		"role":          "assistant",
		"model":         c.opts.Model,
		"content":       content,
		"stop_reason":   c.stopReason,
		"stop_sequence": nil,
		"usage":         c.usageMap(),
	})
	return data
}

func (c *Converter) withMessageStart(events []SSEEvent) []SSEEvent {
	if len(events) == 0 || c.started {
		return events
	}
	c.started = true
	start := newSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            c.opts.MessageID,
			"type":          "message",
			"role":          "assistant",
			"model":         c.opts.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageMap(c.opts.CachePlan.split(c.opts.EstimatedInputTokens)),
		},
	})
	return append([]SSEEvent{start}, events...)
}

func (c *Converter) usageMap() map[string]any {
	return usageMap(c.usage)
}

func usageMap(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": u.CacheCreation5mTokens,
			"ephemeral_1h_input_tokens": u.CacheCreation1hTokens,
		},
	}
}

func (c *Converter) computeUsage() Usage {
	output := c.opts.CountTokens(c.outputText.String())
	for _, b := range c.blocks {
		if b.kind == blockToolUse {
			output += c.opts.CountTokens(b.input.String())
		}
	}
	input := c.opts.EstimatedInputTokens
	if c.contextUsagePercentage > 0 {
		total := int(math.Round(float64(ContextWindow(c.opts.UpstreamModel)) * c.contextUsagePercentage / 100))
		input = max(total-output, 0)
	}
	u := c.opts.CachePlan.split(input)
	u.OutputTokens = output
	return u
}

// ---- 内容块 ----

func (c *Converter) startBlock(kind blockKind, out *[]SSEEvent) *contentBlock {
	c.closeBlock(out)
	b := &contentBlock{kind: kind}
	var initial map[string]any
	switch kind {
	case blockText:
		initial = map[string]any{"type": "text", "text": ""}
	case blockThinking:
		initial = map[string]any{"type": "thinking", "thinking": ""}
	}
	c.blocks = append(c.blocks, b)
	c.open = b
	*out = append(*out, newSSE("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         len(c.blocks) - 1,
		"content_block": initial,
	}))
	return b
}

func (c *Converter) closeBlock(out *[]SSEEvent) {
	if c.open == nil {
		return
	}
	*out = append(*out, newSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": c.blockIndex(c.open),
	}))
	c.open = nil
}

func (c *Converter) blockIndex(b *contentBlock) int {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		if c.blocks[i] == b {
			return i
		}
	}
	return -1
}

// appendText 向正文或思考块追加文本，必要时先关闭其他块。
func (c *Converter) appendText(kind blockKind, s string, out *[]SSEEvent) {
	if s == "" {
		return
	}
	b := c.open
	if b == nil || b.kind != kind {
		b = c.startBlock(kind, out)
	}
	b.text.WriteString(s)
	c.outputText.WriteString(s)
	delta := map[string]any{"type": "text_delta", "text": s}
	if kind == blockThinking {
		c.hasThinking = true
		delta = map[string]any{"type": "thinking_delta", "thinking": s}
	} else if strings.TrimSpace(s) != "" {
		c.hasVisibleText = true
	}
	*out = append(*out, newSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": len(c.blocks) - 1,
		"delta": delta,
	}))
}

// ---- 工具调用 ----

// handleToolUse 处理 toolUseEvent。同一个调用会分多次到达：首个事件带
// name 和 toolUseId，之后是 input 增量，最后一个带 stop。
func (c *Converter) handleToolUse(ev Event, out *[]SSEEvent) {
	if ev.ToolUseID != "" {
		if _, ignored := c.ignoredTools[ev.ToolUseID]; ignored {
			return
		}
		if ev.ToolName == PlaceholderToolName {
			c.ignoredTools[ev.ToolUseID] = struct{}{}
			return
		}
	}

	tool := c.currentTool()
	if ev.ToolUseID != "" && (tool == nil || tool.id != ev.ToolUseID) {
		if ev.ToolName == "" && tool == nil {
			return
		}
		c.flushBeforeTool(out)
		c.closeBlock(out)
		tool = &contentBlock{kind: blockToolUse, id: ev.ToolUseID, name: c.opts.Tools.Original(ev.ToolName)}
		c.blocks = append(c.blocks, tool)
		c.open = tool
		c.toolCount++
		*out = append(*out, newSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": len(c.blocks) - 1,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tool.id,
				"name":  tool.name,
				"input": map[string]any{},
			},
		}))
	}
	if tool == nil {
		return
	}
	if ev.ToolInput != "" {
		tool.input.WriteString(ev.ToolInput)
		*out = append(*out, newSSE("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": c.blockIndex(tool),
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolInput},
		}))
	}
	if ev.ToolStop {
		c.closeBlock(out)
	}
}

func (c *Converter) currentTool() *contentBlock {
	if c.open != nil && c.open.kind == blockToolUse {
		return c.open
	}
	return nil
}

func (c *Converter) closeTool(out *[]SSEEvent) {
	if c.currentTool() != nil {
		c.closeBlock(out)
	}
}

func toolInputObject(raw string) json.RawMessage {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed) {
		return trimmed
	}
	return json.RawMessage(`{}`)
}

// ---- <thinking> 标签 ----

// processThinkingTags 从缓冲区中拆出 <thinking> 内容。标签可能跨事件到达，
// 缓冲区尾部不足一个标签长度的内容留待下次判断。
func (c *Converter) processThinkingTags(out *[]SSEEvent) {
	for c.buf != "" {
		switch {
		case !c.inThinking && !c.thinkingExtracted:
			if pos := findRealTag(c.buf, thinkingStartTag, 0); pos >= 0 {
				before := c.pendingBeforeThinking + c.buf[:pos]
				c.pendingBeforeThinking = ""
				if c.hasVisibleText || strings.TrimSpace(before) != "" {
					// 思考只出现在回复开头。正文之后的标签是模型在正文里提到它，
					// 按正文输出，否则后面的回答都会被当成思考内容。
					c.buf = before + c.buf[pos:]
					c.thinkingExtracted = true
					continue
				}
				c.buf = c.buf[pos+len(thinkingStartTag):]
				c.inThinking = true
				c.stripThinkingNewline = true
				continue
			}
			if safe := safePrefixLen(c.buf, len(thinkingStartTag)); safe > 0 {
				part := c.buf[:safe]
				c.buf = c.buf[safe:]
				if strings.TrimSpace(part) == "" {
					// 前导空白先缓存，避免在思考块前面生成空正文块。
					if room := maxPendingWhitespace - len(c.pendingBeforeThinking); room > 0 {
						c.pendingBeforeThinking += part[:min(room, len(part))]
					}
				} else {
					c.appendText(blockText, c.pendingBeforeThinking+part, out)
					c.pendingBeforeThinking = ""
				}
			}
			return
		case c.inThinking:
			c.stripLeadingThinkingNewline()
			if pos := findThinkingEnd(c.buf); pos >= 0 {
				c.appendText(blockThinking, c.buf[:pos], out)
				c.buf = c.buf[pos+len(thinkingEndTag):]
				c.inThinking = false
				c.thinkingExtracted = true
				c.stripThinkingNewline = false
				if c.open != nil && c.open.kind == blockThinking {
					c.closeBlock(out)
				}
				c.stripTextNewlines = true
				continue
			}
			if safe := safePrefixLen(c.buf, len(thinkingEndTag)); safe > 0 {
				c.appendText(blockThinking, c.buf[:safe], out)
				c.buf = c.buf[safe:]
			}
			return
		default:
			rest := c.buf
			c.buf = ""
			rest = c.stripNewlinesAfterThinking(rest)
			c.appendText(blockText, rest, out)
			return
		}
	}
}

// flushThinkingTags 在流结束时输出缓冲区剩余内容。
func (c *Converter) flushThinkingTags(out *[]SSEEvent) {
	switch {
	case c.inThinking:
		c.stripLeadingThinkingNewline()
		c.appendText(blockThinking, c.buf, out)
	case !c.thinkingExtracted:
		c.appendText(blockText, c.pendingBeforeThinking+c.buf, out)
		c.pendingBeforeThinking = ""
	default:
		c.appendText(blockText, c.stripNewlinesAfterThinking(c.buf), out)
	}
	c.buf = ""
}

// flushBeforeTool 在工具块开始前输出缓冲区内容。工具调用开始意味着正文
// 中不会再出现跨事件的标签，思考状态保持不变。
func (c *Converter) flushBeforeTool(out *[]SSEEvent) {
	if c.inThinking {
		c.stripLeadingThinkingNewline()
		c.appendText(blockThinking, c.buf, out)
		c.buf = ""
		return
	}
	rest := c.pendingBeforeThinking + c.buf
	c.pendingBeforeThinking = ""
	c.buf = ""
	if strings.TrimSpace(rest) != "" {
		c.appendText(blockText, rest, out)
	}
}

func (c *Converter) stripLeadingThinkingNewline() {
	if !c.stripThinkingNewline || c.buf == "" {
		return
	}
	if strings.HasPrefix(c.buf, "\r\n") {
		c.buf = c.buf[2:]
	} else if strings.HasPrefix(c.buf, "\n") {
		c.buf = c.buf[1:]
	} else if c.buf == "\r" {
		return
	}
	c.stripThinkingNewline = false
}

// stripNewlinesAfterThinking 去掉 </thinking> 之后的一个空行。
func (c *Converter) stripNewlinesAfterThinking(s string) string {
	if !c.stripTextNewlines || s == "" {
		return s
	}
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if strings.HasPrefix(s, sep) {
			c.stripTextNewlines = false
			return s[len(sep):]
		}
	}
	if strings.HasPrefix("\r\n\r\n", s) || strings.HasPrefix("\n\n", s) {
		// 空行被拆到了下一个事件，留在缓冲区等待。
		c.buf = s
		return ""
	}
	c.stripTextNewlines = false
	return s
}

// safePrefixLen 返回可以安全输出的前缀长度：保留 keep 字节以免截断标签，
// 并落在 UTF-8 字符边界上。
func safePrefixLen(s string, keep int) int {
	n := len(s) - keep
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func isQuoteAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	switch s[i] {
	case '"', '\'', '`':
		return true
	}
	return false
}

// findRealTag 查找不在引号或反引号内的标签，避免把模型在正文里提到的
// 标签当成真正的分隔符。
func findRealTag(s, tag string, from int) int {
	for from <= len(s) {
		i := strings.Index(s[from:], tag)
		if i < 0 {
			return -1
		}
		pos := from + i
		if !isQuoteAt(s, pos-1) && !isQuoteAt(s, pos+len(tag)) {
			return pos
		}
		from = pos + 1
	}
	return -1
}

// findThinkingEnd 查找真正的 </thinking>：其后紧跟空行，或位于缓冲区末尾
// （后面只有空白）。
func findThinkingEnd(s string) int {
	for from := 0; ; {
		pos := findRealTag(s, thinkingEndTag, from)
		if pos < 0 {
			return -1
		}
		after := s[pos+len(thinkingEndTag):]
		if strings.HasPrefix(after, "\n\n") || strings.HasPrefix(after, "\r\n\r\n") || strings.TrimSpace(after) == "" {
			return pos
		}
		from = pos + 1
	}
}

// ---- 工具函数 ----

func newSSE(event string, data map[string]any) SSEEvent {
	b, _ := json.Marshal(data)
	return SSEEvent{Event: event, Data: b}
}

// roughTokenCount 是没有分词器时的估算：ASCII 约 4 字符一个 token，
// 其他字符各算一个。
func roughTokenCount(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
