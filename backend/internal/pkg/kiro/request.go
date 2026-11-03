package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	generateQURLTemplate             = "https://q.%s.amazonaws.com/generateAssistantResponse"
	generateCodeWhispererURLTemplate = "https://codewhisperer.%s.amazonaws.com/generateAssistantResponse"
	generateTarget                   = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"

	// 生成接口按 Amazon Q CLI 的 SDK 标识发送；profile 查询与用量查询按 Kiro IDE 发送。
	cliAmzUserAgent = "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.17593 os/linux lang/rust/1.92.0 m/F app/AmazonQ-For-CLI"
	cliUserAgent    = "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.17593 os/linux lang/rust/1.92.0 md/appVersion-2.10.0 app/AmazonQ-For-CLI"
	ideVersion      = "0.11.63"

	originAIEditor    = "AI_EDITOR"
	chatTriggerManual = "MANUAL"
	agentTaskVibe     = "vibe"

	// maxToolNameLength 是 Kiro 接受的工具名最大长度。
	maxToolNameLength = 64
	// maxToolDescriptionLength 是工具描述的截断长度。
	maxToolDescriptionLength = 9216
	// historyImageWindow 是保留图片的历史消息范围（距最后一条消息的条数）。
	historyImageWindow = 5

	// PlaceholderToolName 是没有可用工具时注入的占位工具。Kiro 在 tools 为空时报错。
	PlaceholderToolName = "no_tool_available"
	placeholderToolDesc = "Internal no-op placeholder. Never call this tool (or any tool) in this turn. Do not announce or promise actions such as reading files or running commands. Instead, write your full and complete answer directly as natural-language text in this single reply, based on the information already available to you."

	thinkingMinBudget     = 1024
	thinkingMaxBudget     = 24576
	thinkingDefaultBudget = 20000

	continueText    = "Continue"
	toolResultsText = "Tool results provided."
	imageOnlyText   = "Image provided."
	// emptyToolResultText 填充没有文本的工具结果，Kiro 不接受空文本。
	emptyToolResultText = "(empty result)"
)

// GenerateURL 返回生成接口地址。q.* 端点要求 profileArn；没有 profile 的
// idc 凭据（常见于 Builder ID）走 codewhisperer.* 端点。
func GenerateURL(creds Credentials) string {
	if creds.AuthMethod == AuthMethodIdC && strings.TrimSpace(creds.ProfileARN) == "" {
		return fmt.Sprintf(generateCodeWhispererURLTemplate, creds.APIRegion())
	}
	return fmt.Sprintf(generateQURLTemplate, creds.APIRegion())
}

// NewGenerateRequest 构造 generateAssistantResponse 请求。
func NewGenerateRequest(ctx context.Context, creds Credentials, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GenerateURL(creds), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("x-amz-target", generateTarget)
	req.Header.Set("x-amz-user-agent", cliAmzUserAgent)
	req.Header.Set("User-Agent", cliUserAgent)
	return req, nil
}

// ideHeaders 返回 Kiro IDE 风格的 SDK 请求头。
func ideHeaders(creds Credentials) http.Header {
	key := firstNonEmpty(creds.ProfileARN, creds.ClientID, "KIRO_DEFAULT_MACHINE")
	sum := sha256.Sum256([]byte(key))
	machineID := hex.EncodeToString(sum[:])
	h := http.Header{}
	h.Set("amz-sdk-invocation-id", uuid.NewString())
	h.Set("amz-sdk-request", "attempt=1; max=1")
	h.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-"+ideVersion+"-"+machineID)
	h.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/linux lang/js md/nodejs#20.18.0 api/codewhisperer#1.0.34 m/E KiroIDE-"+ideVersion+"-"+machineID)
	return h
}

// BuildOptions 控制请求转换。
type BuildOptions struct {
	// Model 是账号映射后的模型名，客户端写法与 Kiro 写法均可。
	Model string
	// ProfileARN 为空时请求体不带 profileArn。
	ProfileARN string
	// ConversationID 为空时自动生成。
	ConversationID string
}

// BuiltRequest 是转换后的 Kiro 请求。
type BuiltRequest struct {
	Body              []byte
	UpstreamModel     string
	ThinkingRequested bool
	Tools             *ToolNames
}

// BuildRequest 把 Anthropic Messages 请求体转换为 Kiro conversationState。
//
// Kiro 对请求形态有几条硬性要求：currentMessage 必须是 user 消息；history
// 非空时必须以 assistant 结尾；不能出现 null 和空数组；tools 不能为空；同一
// 消息里 toolUseId 不能重复。转换按这些要求整理消息。
func BuildRequest(claudeBody []byte, opts BuildOptions) (*BuiltRequest, error) {
	var req claudeRequest
	if err := json.Unmarshal(claudeBody, &req); err != nil {
		return nil, fmt.Errorf("parse messages request: %w", err)
	}
	messages := normalizeMessages(req.Messages)
	if len(messages) == 0 {
		return nil, errors.New("messages is empty")
	}

	upstreamModel := UpstreamModelID(opts.Model)
	tools := newToolNames(req.Tools)
	thinkingRequested := req.Thinking != nil && (strings.EqualFold(req.Thinking.Type, "enabled") || strings.EqualFold(req.Thinking.Type, "adaptive"))

	system := systemText(req.System)
	if prefix := thinkingPrefix(req.Thinking, req.effort()); prefix != "" && !strings.Contains(system, "<thinking_mode>") {
		if system == "" {
			system = prefix
		} else {
			system = prefix + "\n" + system
		}
	}

	b := historyBuilder{model: upstreamModel, tools: tools, total: len(messages)}
	last := len(messages) - 1
	prependSystemToCurrent := false
	switch {
	case system == "":
	case messages[0].Role == "user" && len(messages) == 1:
		prependSystemToCurrent = true
	case messages[0].Role == "user":
		b.systemForFirstUser = system
	default:
		b.appendUser(&kiroUserInput{Content: system, ModelID: upstreamModel, Origin: originAIEditor})
	}
	for i := 0; i < last; i++ {
		b.appendMessage(i, messages[i])
	}

	var current *kiroUserInput
	if messages[last].Role == "assistant" {
		b.appendMessage(last, messages[last])
		current = &kiroUserInput{Content: continueText, ModelID: upstreamModel, Origin: originAIEditor}
	} else {
		current = b.userInput(last, messages[last], true)
		if prependSystemToCurrent {
			current.Content = system + "\n\n" + current.Content
		}
	}
	history := b.finish()

	if current.UserInputMessageContext == nil {
		current.UserInputMessageContext = &kiroUserContext{}
	}
	current.UserInputMessageContext.Tools = buildToolEntries(req.Tools, tools)

	payload := kiroPayload{
		ConversationState: kiroConversationState{
			AgentTaskType:   agentTaskVibe,
			ChatTriggerType: chatTriggerManual,
			ConversationID:  firstNonEmpty(opts.ConversationID, uuid.NewString()),
			History:         history,
			CurrentMessage:  kiroCurrentMessage{UserInputMessage: current},
		},
		AdditionalModelRequestFields: additionalModelRequestFields(opts.Model, req.Thinking, req.effort()),
		ProfileARN:                   strings.TrimSpace(opts.ProfileARN),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode kiro request: %w", err)
	}
	return &BuiltRequest{
		Body:              body,
		UpstreamModel:     upstreamModel,
		ThinkingRequested: thinkingRequested,
		Tools:             tools,
	}, nil
}

// ---- Anthropic 请求结构 ----

type claudeRequest struct {
	System       json.RawMessage `json:"system"`
	Messages     []claudeMessage `json:"messages"`
	Tools        []claudeTool    `json:"tools"`
	Thinking     *claudeThinking `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func (r *claudeRequest) effort() string {
	effort := r.ReasoningEffort
	if effort == "" && r.OutputConfig != nil {
		effort = r.OutputConfig.Effort
	}
	return strings.ToLower(strings.TrimSpace(effort))
}

type claudeThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

type normMessage struct {
	Role   string
	Blocks []claudeBlock
}

// ---- Kiro 请求结构 ----

type kiroPayload struct {
	ConversationState            kiroConversationState `json:"conversationState"`
	AdditionalModelRequestFields map[string]any        `json:"additionalModelRequestFields,omitempty"`
	ProfileARN                   string                `json:"profileArn,omitempty"`
}

type kiroConversationState struct {
	AgentTaskType   string             `json:"agentTaskType"`
	ChatTriggerType string             `json:"chatTriggerType"`
	ConversationID  string             `json:"conversationId"`
	History         []kiroHistoryItem  `json:"history,omitempty"`
	CurrentMessage  kiroCurrentMessage `json:"currentMessage"`
}

type kiroCurrentMessage struct {
	UserInputMessage *kiroUserInput `json:"userInputMessage"`
}

type kiroHistoryItem struct {
	UserInputMessage         *kiroUserInput      `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *kiroAssistantInput `json:"assistantResponseMessage,omitempty"`
}

type kiroUserInput struct {
	Content                 string           `json:"content"`
	ModelID                 string           `json:"modelId"`
	Origin                  string           `json:"origin"`
	Images                  []kiroImage      `json:"images,omitempty"`
	UserInputMessageContext *kiroUserContext `json:"userInputMessageContext,omitempty"`
}

type kiroUserContext struct {
	ToolResults []kiroToolResult `json:"toolResults,omitempty"`
	Tools       []kiroToolEntry  `json:"tools,omitempty"`
}

type kiroToolEntry struct {
	ToolSpecification *kiroToolSpec   `json:"toolSpecification,omitempty"`
	CachePoint        *kiroCachePoint `json:"cachePoint,omitempty"`
}

type kiroToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema kiroInputSchema `json:"inputSchema"`
}

type kiroInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type kiroCachePoint struct {
	Type string `json:"type"`
}

type kiroToolResult struct {
	Content   []kiroText `json:"content"`
	Status    string     `json:"status"`
	ToolUseID string     `json:"toolUseId"`
}

type kiroText struct {
	Text string `json:"text"`
}

type kiroImage struct {
	Format string          `json:"format"`
	Source kiroImageSource `json:"source"`
}

type kiroImageSource struct {
	Bytes string `json:"bytes"`
}

type kiroAssistantInput struct {
	Content  string        `json:"content"`
	ToolUses []kiroToolUse `json:"toolUses,omitempty"`
}

type kiroToolUse struct {
	Input     json.RawMessage `json:"input"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"toolUseId"`
}

// ---- 消息整理 ----

// normalizeMessages 把 content 统一成块数组，去掉 Claude Code 偶发的末尾
// "{" 助手消息和实质为空的轮次，再合并相邻同角色消息。
func normalizeMessages(raw []claudeMessage) []normMessage {
	out := make([]normMessage, 0, len(raw))
	for _, m := range raw {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, normMessage{Role: role, Blocks: contentBlocks(m.Content)})
	}
	if n := len(out); n > 0 && out[n-1].Role == "assistant" && strings.TrimSpace(blocksText(out[n-1].Blocks)) == "{" && onlyTextBlocks(out[n-1].Blocks) {
		out = out[:n-1]
	}
	if len(out) > 1 {
		kept := out[:0:0]
		for _, m := range out {
			if !isEmptyMessage(m) {
				kept = append(kept, m)
			}
		}
		if len(kept) > 0 {
			out = kept
		}
	}
	merged := make([]normMessage, 0, len(out))
	for _, m := range out {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Blocks = append(merged[n-1].Blocks, m.Blocks...)
			continue
		}
		merged = append(merged, m)
	}
	return merged
}

func contentBlocks(raw json.RawMessage) []claudeBlock {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []claudeBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

func isEmptyMessage(m normMessage) bool {
	for _, b := range m.Blocks {
		if b.Type != "text" || strings.TrimSpace(b.Text) != "" {
			return false
		}
	}
	return true
}

func onlyTextBlocks(blocks []claudeBlock) bool {
	for _, b := range blocks {
		if b.Type != "text" {
			return false
		}
	}
	return len(blocks) > 0
}

func blocksText(blocks []claudeBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func systemText(raw json.RawMessage) string {
	return strings.TrimSpace(blocksText(contentBlocks(raw)))
}

// toolResultText 把 tool_result.content（字符串或块数组）转换成文本。Kiro 的
// toolResults 只接受文本：图片交给 keepImage 放到所在 user 消息的 images 里
// （Claude Code 的 Read 读图片、浏览器截图都这样返回），文本里注明；没保留的
// 图片和文档（如 Read 读 PDF）无法转发，也注明，否则模型只会看到一个空结果。
// Kiro 不接受空文本。
func toolResultText(raw json.RawMessage, keepImage func(claudeBlock) bool) string {
	blocks := contentBlocks(raw)
	text := blocksText(blocks)
	attached, dropped, documents := 0, 0, 0
	for _, b := range blocks {
		switch b.Type {
		case "image":
			if keepImage(b) {
				attached++
			} else {
				dropped++
			}
		case "document":
			documents++
		}
	}
	if attached > 0 {
		text = joinNonEmpty(text, "["+strconv.Itoa(attached)+" image(s) from this tool result are attached to this message]", "\n")
	}
	if dropped > 0 {
		text = joinNonEmpty(text, "["+strconv.Itoa(dropped)+" image(s) from this tool result omitted from history]", "\n")
	}
	if documents > 0 {
		text = joinNonEmpty(text, "["+strconv.Itoa(documents)+" document(s) omitted: documents are not supported]", "\n")
	}
	if strings.TrimSpace(text) == "" {
		text = emptyToolResultText
	}
	return text
}

// ---- history 构建 ----

type historyBuilder struct {
	model string
	tools *ToolNames
	total int
	// systemForFirstUser 非空时拼到第一条 user 消息前面。
	systemForFirstUser string
	items              []kiroHistoryItem
}

func (b *historyBuilder) appendMessage(index int, m normMessage) {
	if m.Role == "assistant" {
		if msg := b.assistantInput(m); msg != nil {
			b.items = append(b.items, kiroHistoryItem{AssistantResponseMessage: msg})
		}
		return
	}
	msg := b.userInput(index, m, false)
	if msg != nil {
		b.appendUser(msg)
	}
}

func (b *historyBuilder) appendUser(msg *kiroUserInput) {
	b.items = append(b.items, kiroHistoryItem{UserInputMessage: msg})
}

// userInput 转换一条 user 消息。current 为 true 时用于 currentMessage：
// 图片全部保留，内容为空时填占位文本而不是跳过。
func (b *historyBuilder) userInput(index int, m normMessage, current bool) *kiroUserInput {
	keepImages := current || (b.total-1)-index <= historyImageWindow
	var (
		texts     []string
		images    []kiroImage
		omitted   int
		documents int
		results   []kiroToolResult
		resultIDs = map[string]struct{}{}
	)
	keepImage := func(block claudeBlock) bool {
		img, ok := toKiroImage(block)
		if !ok || !keepImages {
			return false
		}
		images = append(images, img)
		return true
	}
	for _, block := range m.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "tool_result":
			if _, dup := resultIDs[block.ToolUseID]; dup {
				continue
			}
			resultIDs[block.ToolUseID] = struct{}{}
			status := "success"
			if block.IsError {
				status = "error"
			}
			results = append(results, kiroToolResult{
				Content:   []kiroText{{Text: toolResultText(block.Content, keepImage)}},
				Status:    status,
				ToolUseID: block.ToolUseID,
			})
		case "image":
			if !keepImage(block) {
				omitted++
			}
		case "document":
			documents++
		}
	}
	content := strings.Join(texts, "\n")
	if omitted > 0 {
		content = joinNonEmpty(content, "[此消息包含 "+strconv.Itoa(omitted)+" 张图片，已在历史记录中省略]", "\n")
	}
	if documents > 0 {
		content = joinNonEmpty(content, "["+strconv.Itoa(documents)+" document(s) omitted: documents are not supported]", "\n")
	}
	if index == 0 && b.systemForFirstUser != "" {
		if strings.TrimSpace(content) == "" {
			content = b.systemForFirstUser
		} else {
			content = b.systemForFirstUser + "\n\n" + content
		}
	}
	if strings.TrimSpace(content) == "" {
		switch {
		case len(results) > 0:
			content = toolResultsText
		case len(images) > 0:
			content = imageOnlyText
		case current:
			content = continueText
		default:
			return nil
		}
	}
	msg := &kiroUserInput{Content: content, ModelID: b.model, Origin: originAIEditor, Images: images}
	if len(results) > 0 {
		msg.UserInputMessageContext = &kiroUserContext{ToolResults: results}
	}
	return msg
}

func (b *historyBuilder) assistantInput(m normMessage) *kiroAssistantInput {
	var (
		texts    []string
		thinking strings.Builder
		toolUses []kiroToolUse
	)
	for _, block := range m.Blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "thinking":
			thinking.WriteString(firstNonEmptyRaw(block.Thinking, block.Text))
		case "tool_use":
			toolUses = append(toolUses, kiroToolUse{
				Input:     sanitizeToolInput(block.Input),
				Name:      b.tools.Upstream(block.Name),
				ToolUseID: block.ID,
			})
		}
	}
	content := strings.Join(texts, "\n")
	if thinking.Len() > 0 {
		wrapped := "<thinking>" + thinking.String() + "</thinking>"
		if content == "" {
			content = wrapped
		} else {
			content = wrapped + "\n\n" + content
		}
	}
	if strings.TrimSpace(content) == "" && len(toolUses) == 0 {
		return nil
	}
	return &kiroAssistantInput{Content: content, ToolUses: toolUses}
}

// finish 合并相邻同类型条目，并保证 history 以 assistant 结尾。
func (b *historyBuilder) finish() []kiroHistoryItem {
	merged := make([]kiroHistoryItem, 0, len(b.items)+1)
	for _, item := range b.items {
		n := len(merged)
		if n == 0 {
			merged = append(merged, item)
			continue
		}
		prev := &merged[n-1]
		switch {
		case prev.UserInputMessage != nil && item.UserInputMessage != nil:
			mergeUserInput(prev.UserInputMessage, item.UserInputMessage)
		case prev.AssistantResponseMessage != nil && item.AssistantResponseMessage != nil:
			p, cur := prev.AssistantResponseMessage, item.AssistantResponseMessage
			p.Content = joinNonEmpty(p.Content, cur.Content, "\n\n")
			p.ToolUses = append(p.ToolUses, cur.ToolUses...)
		default:
			merged = append(merged, item)
		}
	}
	if n := len(merged); n > 0 && merged[n-1].AssistantResponseMessage == nil {
		merged = append(merged, kiroHistoryItem{AssistantResponseMessage: &kiroAssistantInput{Content: continueText}})
	}
	return merged
}

func mergeUserInput(dst, src *kiroUserInput) {
	dst.Content = joinNonEmpty(dst.Content, src.Content, "\n\n")
	dst.Images = append(dst.Images, src.Images...)
	if src.UserInputMessageContext == nil || len(src.UserInputMessageContext.ToolResults) == 0 {
		return
	}
	if dst.UserInputMessageContext == nil {
		dst.UserInputMessageContext = &kiroUserContext{}
	}
	seen := make(map[string]struct{}, len(dst.UserInputMessageContext.ToolResults))
	for _, r := range dst.UserInputMessageContext.ToolResults {
		seen[r.ToolUseID] = struct{}{}
	}
	for _, r := range src.UserInputMessageContext.ToolResults {
		if _, dup := seen[r.ToolUseID]; dup {
			continue
		}
		seen[r.ToolUseID] = struct{}{}
		dst.UserInputMessageContext.ToolResults = append(dst.UserInputMessageContext.ToolResults, r)
	}
}

func joinNonEmpty(a, b, sep string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + sep + b
	}
}

func firstNonEmptyRaw(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func toKiroImage(block claudeBlock) (kiroImage, bool) {
	if block.Source == nil || block.Source.Data == "" || (block.Source.Type != "" && block.Source.Type != "base64") {
		return kiroImage{}, false
	}
	format := strings.TrimPrefix(strings.ToLower(block.Source.MediaType), "image/")
	if format == "" {
		format = "png"
	}
	return kiroImage{Format: format, Source: kiroImageSource{Bytes: block.Source.Data}}, true
}

// sanitizeToolInput 保证 tool_use.input 是 JSON 对象，并去掉空字符串键。
func sanitizeToolInput(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			raw = bytes.TrimSpace([]byte(s))
		}
	}
	var obj map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil {
		return json.RawMessage(`{}`)
	}
	if _, ok := obj[""]; !ok {
		return raw
	}
	delete(obj, "")
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

// ---- 工具 ----

// ToolNames 记录工具名在 Kiro 侧的写法。超过 64 个字符的工具名会被缩短，
// 响应中需要还原。
type ToolNames struct {
	toUpstream   map[string]string
	fromUpstream map[string]string
}

func newToolNames(tools []claudeTool) *ToolNames {
	t := &ToolNames{toUpstream: map[string]string{}, fromUpstream: map[string]string{}}
	for _, tool := range tools {
		if tool.Name == "" {
			continue
		}
		short := shortenToolName(tool.Name)
		t.toUpstream[tool.Name] = short
		if short != tool.Name {
			t.fromUpstream[short] = tool.Name
		}
	}
	return t
}

// Upstream 返回发给 Kiro 的工具名。
func (t *ToolNames) Upstream(name string) string {
	if t != nil {
		if short, ok := t.toUpstream[name]; ok {
			return short
		}
	}
	return shortenToolName(name)
}

// Original 把 Kiro 返回的工具名还原成客户端的工具名。
func (t *ToolNames) Original(name string) string {
	if t != nil {
		if original, ok := t.fromUpstream[name]; ok {
			return original
		}
	}
	return name
}

func shortenToolName(name string) string {
	runes := []rune(name)
	if len(runes) <= maxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:12]
	return string(runes[:maxToolNameLength-len(suffix)-1]) + "_" + suffix
}

func buildToolEntries(tools []claudeTool, names *ToolNames) []kiroToolEntry {
	entries := make([]kiroToolEntry, 0, len(tools)+1)
	for _, tool := range tools {
		lower := strings.ToLower(tool.Name)
		if lower == "web_search" || lower == "websearch" {
			continue
		}
		desc := tool.Description
		if strings.TrimSpace(desc) == "" {
			// Kiro 不接受空描述。OpenAI 的 function.description 是可选的，直接丢掉
			// 会让模型无法调用该工具，历史里的 tool_use 也找不到对应定义。
			desc = tool.Name
		}
		if runes := []rune(desc); len(runes) > maxToolDescriptionLength {
			desc = string(runes[:maxToolDescriptionLength]) + "..."
		}
		schema := bytes.TrimSpace(tool.InputSchema)
		if len(schema) == 0 || bytes.Equal(schema, []byte("null")) {
			schema = json.RawMessage(`{}`)
		}
		entries = append(entries, kiroToolEntry{ToolSpecification: &kiroToolSpec{
			Name:        names.Upstream(tool.Name),
			Description: desc,
			InputSchema: kiroInputSchema{JSON: schema},
		}})
		if len(bytes.TrimSpace(tool.CacheControl)) > 0 && !bytes.Equal(bytes.TrimSpace(tool.CacheControl), []byte("null")) {
			entries = append(entries, kiroToolEntry{CachePoint: &kiroCachePoint{Type: "default"}})
		}
	}
	if len(entries) == 0 {
		entries = append(entries, kiroToolEntry{ToolSpecification: &kiroToolSpec{
			Name:        PlaceholderToolName,
			Description: placeholderToolDesc,
			InputSchema: kiroInputSchema{JSON: json.RawMessage(`{"type":"object","properties":{}}`)},
		}})
	}
	return entries
}

// ---- thinking 与 effort ----

// thinkingPrefix 生成放在 system 前的思考模式标记。
func thinkingPrefix(thinking *claudeThinking, effort string) string {
	if thinking == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "enabled":
		budget := thinking.BudgetTokens
		if budget <= 0 {
			budget = thinkingDefaultBudget
		}
		budget = max(budget, thinkingMinBudget)
		budget = min(budget, thinkingMaxBudget)
		return "<thinking_mode>enabled</thinking_mode><max_thinking_length>" + strconv.Itoa(budget) + "</max_thinking_length>"
	case "adaptive":
		level := "high"
		if effort == "low" || effort == "medium" {
			level = effort
		}
		return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + level + "</thinking_effort>"
	}
	return ""
}

// additionalModelRequestFields 生成 effort 扩展字段。Kiro 不忽略未知字段，
// 不支持 effort 的模型必须省略整个字段，否则返回 400。
func additionalModelRequestFields(model string, thinking *claudeThinking, effort string) map[string]any {
	id := canonicalModel(model)
	if id == "" {
		return nil
	}
	spec := models[id]
	if len(spec.EffortLevels) == 0 {
		return nil
	}
	thinkingType := ""
	if thinking != nil {
		thinkingType = strings.ToLower(strings.TrimSpace(thinking.Type))
	}
	supported := func(level string) bool {
		for _, l := range spec.EffortLevels {
			if l == level {
				return true
			}
		}
		return false
	}
	switch spec.EffortField {
	case effortFieldReasoning:
		level := ""
		switch {
		case thinkingType == "disabled":
			level = "none"
		case effort != "" && supported(effort):
			level = effort
		case effort != "":
			level = "high"
		}
		if level == "" {
			return nil
		}
		return map[string]any{"reasoning": map[string]any{"effort": level}}
	case effortFieldOutputConfig:
		level := ""
		switch {
		case effort != "" && supported(effort):
			level = effort
		case effort != "":
			level = "medium"
		case thinkingType == "enabled" || thinkingType == "adaptive":
			level = "medium"
		}
		if level == "" {
			return nil
		}
		return map[string]any{"output_config": map[string]any{"effort": level}}
	}
	return nil
}
