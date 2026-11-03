package kiro

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func build(t *testing.T, body string, opts BuildOptions) gjson.Result {
	t.Helper()
	if opts.ConversationID == "" {
		opts.ConversationID = "conv-1"
	}
	built, err := BuildRequest([]byte(body), opts)
	require.NoError(t, err)
	require.NotContains(t, string(built.Body), "null", "Kiro rejects null fields")
	return gjson.ParseBytes(built.Body)
}

func TestBuildRequestSingleMessageWithSystem(t *testing.T) {
	t.Parallel()
	r := build(t, `{
		"model": "claude-sonnet-4-5",
		"system": [{"type":"text","text":"You are helpful."}],
		"messages": [{"role":"user","content":"hi"}]
	}`, BuildOptions{Model: "claude-sonnet-4-5", ProfileARN: "arn:p"})

	cs := r.Get("conversationState")
	require.Equal(t, "vibe", cs.Get("agentTaskType").String())
	require.Equal(t, "MANUAL", cs.Get("chatTriggerType").String())
	require.Equal(t, "conv-1", cs.Get("conversationId").String())
	require.False(t, cs.Get("history").Exists())
	cur := cs.Get("currentMessage.userInputMessage")
	require.Equal(t, "You are helpful.\n\nhi", cur.Get("content").String())
	require.Equal(t, "claude-sonnet-4.5", cur.Get("modelId").String())
	require.Equal(t, "AI_EDITOR", cur.Get("origin").String())
	require.Equal(t, PlaceholderToolName, cur.Get("userInputMessageContext.tools.0.toolSpecification.name").String())
	require.Equal(t, "arn:p", r.Get("profileArn").String())
	require.False(t, r.Get("additionalModelRequestFields").Exists())
}

func TestBuildRequestHistoryAndToolResults(t *testing.T) {
	t.Parallel()
	r := build(t, `{
		"system": "sys",
		"tools": [{"name":"read_file","description":"Read a file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},
		          {"name":"web_search","description":"search"},
		          {"name":"no_desc","description":""}],
		"messages": [
			{"role":"user","content":"read a.txt"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"need the file"},
				{"type":"text","text":"Reading."},
				{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"a.txt","":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"hello"}]},
				{"type":"tool_result","tool_use_id":"t1","content":"dup"}
			]}
		]
	}`, BuildOptions{Model: "claude-opus-4-7"})

	h := r.Get("conversationState.history")
	require.Len(t, h.Array(), 2)
	require.Equal(t, "sys\n\nread a.txt", h.Get("0.userInputMessage.content").String())
	asst := h.Get("1.assistantResponseMessage")
	require.Equal(t, "<thinking>need the file</thinking>\n\nReading.", asst.Get("content").String())
	require.Equal(t, "read_file", asst.Get("toolUses.0.name").String())
	require.Equal(t, `{"path":"a.txt"}`, asst.Get("toolUses.0.input").Raw)

	cur := r.Get("conversationState.currentMessage.userInputMessage")
	require.Equal(t, "Tool results provided.", cur.Get("content").String())
	results := cur.Get("userInputMessageContext.toolResults").Array()
	require.Len(t, results, 1)
	require.Equal(t, "hello", results[0].Get("content.0.text").String())
	require.Equal(t, "success", results[0].Get("status").String())

	tools := cur.Get("userInputMessageContext.tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "read_file", tools[0].Get("toolSpecification.name").String())
	require.Equal(t, "default", tools[1].Get("cachePoint.type").String())
	// 空描述的工具保留下来，用工具名作描述。
	require.Equal(t, "no_desc", tools[2].Get("toolSpecification.name").String())
	require.Equal(t, "no_desc", tools[2].Get("toolSpecification.description").String())
}

func TestBuildRequestTrailingAssistantBecomesHistory(t *testing.T) {
	t.Parallel()
	r := build(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"assistant","content":"b"}
	]}`, BuildOptions{Model: "claude-haiku-4-5"})
	h := r.Get("conversationState.history").Array()
	require.Len(t, h, 2)
	require.Equal(t, "b", h[1].Get("assistantResponseMessage.content").String())
	require.Equal(t, "Continue", r.Get("conversationState.currentMessage.userInputMessage.content").String())
}

func TestBuildRequestDropsBraceAssistantAndMergesRoles(t *testing.T) {
	t.Parallel()
	r := build(t, `{"messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":[{"type":"text","text":"   "}]},
		{"role":"assistant","content":"b"},
		{"role":"user","content":"c"},
		{"role":"user","content":"d"},
		{"role":"assistant","content":"{"}
	]}`, BuildOptions{Model: "claude-haiku-4-5"})
	h := r.Get("conversationState.history").Array()
	require.Len(t, h, 2)
	require.Equal(t, "a", h[0].Get("userInputMessage.content").String())
	require.Equal(t, "c\nd", r.Get("conversationState.currentMessage.userInputMessage.content").String())
}

func TestBuildRequestHistoryEndsWithAssistant(t *testing.T) {
	t.Parallel()
	// 系统提示词在首条消息为 assistant 时单独成为一条 user 历史。
	r := build(t, `{"system":"s","messages":[
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"hi"}
	]}`, BuildOptions{Model: "claude-haiku-4-5"})
	h := r.Get("conversationState.history").Array()
	require.Len(t, h, 2)
	require.Equal(t, "s", h[0].Get("userInputMessage.content").String())
	require.Equal(t, "hello", h[1].Get("assistantResponseMessage.content").String())
}

func TestBuildRequestOldHistoryImagesAreOmitted(t *testing.T) {
	t.Parallel()
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	msgs := []string{`{"role":"user","content":[{"type":"text","text":"look"},` + img + `]}`}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, `{"role":"assistant","content":"ok"}`, `{"role":"user","content":"more"}`)
	}
	msgs = append(msgs, `{"role":"assistant","content":"ok"}`, `{"role":"user","content":[`+img+`]}`)
	r := build(t, `{"messages":[`+strings.Join(msgs, ",")+`]}`, BuildOptions{Model: "claude-haiku-4-5"})

	first := r.Get("conversationState.history.0.userInputMessage")
	require.False(t, first.Get("images").Exists())
	require.Equal(t, "look\n[此消息包含 1 张图片，已在历史记录中省略]", first.Get("content").String())
	cur := r.Get("conversationState.currentMessage.userInputMessage")
	require.Equal(t, "Image provided.", cur.Get("content").String())
	require.Equal(t, "png", cur.Get("images.0.format").String())
	require.Equal(t, "AAAA", cur.Get("images.0.source.bytes").String())
}

func TestBuildRequestThinkingAndEffort(t *testing.T) {
	t.Parallel()
	r := build(t, `{"thinking":{"type":"enabled","budget_tokens":100000},"messages":[{"role":"user","content":"x"}]}`,
		BuildOptions{Model: "claude-opus-4-7"})
	require.Equal(t, "<thinking_mode>enabled</thinking_mode><max_thinking_length>24576</max_thinking_length>\n\nx",
		r.Get("conversationState.currentMessage.userInputMessage.content").String())
	require.Equal(t, "medium", r.Get("additionalModelRequestFields.output_config.effort").String())

	r = build(t, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"x"}]}`,
		BuildOptions{Model: "claude-sonnet-4-6"})
	require.True(t, strings.HasPrefix(r.Get("conversationState.currentMessage.userInputMessage.content").String(),
		"<thinking_mode>adaptive</thinking_mode><thinking_effort>high</thinking_effort>"))
	require.Equal(t, "medium", r.Get("additionalModelRequestFields.output_config.effort").String(), "sonnet 4.6 has no xhigh")

	r = build(t, `{"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"x"}]}`, BuildOptions{Model: "gpt-5.6-sol"})
	require.Equal(t, "none", r.Get("additionalModelRequestFields.reasoning.effort").String())

	r = build(t, `{"output_config":{"effort":"high"},"messages":[{"role":"user","content":"x"}]}`, BuildOptions{Model: "claude-haiku-4-5"})
	require.False(t, r.Get("additionalModelRequestFields").Exists())
}

func TestBuildRequestLongToolNames(t *testing.T) {
	t.Parallel()
	long := "mcp__" + strings.Repeat("very_long_server_name_", 4) + "tool"
	body := `{"tools":[{"name":"` + long + `","description":"d","input_schema":{}}],"messages":[
		{"role":"user","content":"x"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` + long + `","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":true}]}
	]}`
	built, err := BuildRequest([]byte(body), BuildOptions{Model: "claude-haiku-4-5"})
	require.NoError(t, err)
	r := gjson.ParseBytes(built.Body)
	short := r.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.0.toolSpecification.name").String()
	require.LessOrEqual(t, len(short), 64)
	require.NotEqual(t, long, short)
	require.Equal(t, short, r.Get("conversationState.history.1.assistantResponseMessage.toolUses.0.name").String())
	require.Equal(t, long, built.Tools.Original(short))
	require.Equal(t, "error", r.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0.status").String())
}

func TestBuildRequestRejectsEmptyMessages(t *testing.T) {
	t.Parallel()
	_, err := BuildRequest([]byte(`{"messages":[]}`), BuildOptions{Model: "claude-haiku-4-5"})
	require.Error(t, err)
}

func TestGenerateURLAndHeaders(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		GenerateURL(Credentials{AuthMethod: AuthMethodSocial}))
	require.Equal(t, "https://q.eu-central-1.amazonaws.com/generateAssistantResponse",
		GenerateURL(Credentials{AuthMethod: AuthMethodIdC, ProfileARN: "arn:p", Region: "eu-central-1"}))
	require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		GenerateURL(Credentials{AuthMethod: AuthMethodIdC}))

	req, err := NewGenerateRequest(context.Background(), Credentials{AccessToken: "at", AuthMethod: AuthMethodSocial}, []byte("{}"))
	require.NoError(t, err)
	require.Equal(t, "Bearer at", req.Header.Get("Authorization"))
	require.Equal(t, "AmazonCodeWhispererStreamingService.GenerateAssistantResponse", req.Header.Get("x-amz-target"))
	require.NotEmpty(t, req.Header.Get("amz-sdk-invocation-id"))
}

func TestModelMapping(t *testing.T) {
	t.Parallel()
	require.Equal(t, "claude-opus-4.7", UpstreamModelID("claude-opus-4-7"))
	require.Equal(t, "claude-opus-4.7", UpstreamModelID("claude-opus-4.7"))
	require.Equal(t, "claude-sonnet-4.5", UpstreamModelID("claude-sonnet-4-5-20250929"))
	require.Equal(t, "custom-model", UpstreamModelID("custom-model"))
	require.True(t, IsKnownModel("gpt-5.6-luna"))
	require.False(t, IsKnownModel("claude-3-opus"))
	require.Equal(t, 1_000_000, ContextWindow("claude-opus-4-7"))
	require.Equal(t, 200_000, ContextWindow("unknown"))
	require.Contains(t, DefaultModelIDs(), DefaultTestModel)
}

func TestBuildRequestToolResultImagesAndDocuments(t *testing.T) {
	t.Parallel()
	toolTurn := func(id, content string) string {
		return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + content + `}]}`
	}
	image := `[{"type":"text","text":"shot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}}]`
	filler := ""
	for i := 0; i < historyImageWindow; i++ {
		filler += `,{"role":"assistant","content":"ok"},{"role":"user","content":"next"}`
	}
	r := build(t, `{"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object"}}],"messages":[
		{"role":"user","content":"start"},
		`+toolTurn("old", image)+filler+`,
		`+toolTurn("pdf", `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}}]`)+`,
		`+toolTurn("empty", `""`)+`,
		`+toolTurn("cur", image)+`
	]}`, BuildOptions{Model: "claude-opus-4-7"})

	text := func(item gjson.Result) string {
		return item.Get("userInputMessage.userInputMessageContext.toolResults.0.content.0.text").String()
	}
	history := r.Get("conversationState.history").Array()
	resultByID := map[string]gjson.Result{}
	for _, item := range history {
		if id := item.Get("userInputMessage.userInputMessageContext.toolResults.0.toolUseId").String(); id != "" {
			resultByID[id] = item
		}
	}

	// 超出保留窗口的截图不转发，工具结果里注明。
	old := resultByID["old"]
	require.Equal(t, "shot\n[1 image(s) from this tool result omitted from history]", text(old))
	require.False(t, old.Get("userInputMessage.images").Exists())
	require.NotContains(t, old.Get("userInputMessage.content").String(), "已在历史记录中省略")

	require.Equal(t, "[1 document(s) omitted: documents are not supported]", text(resultByID["pdf"]))
	require.Equal(t, emptyToolResultText, text(resultByID["empty"]))

	// 当前消息的截图放到 images 里。
	cur := r.Get("conversationState.currentMessage")
	require.Equal(t, "shot\n[1 image(s) from this tool result are attached to this message]", text(cur))
	require.Equal(t, "png", cur.Get("userInputMessage.images.0.format").String())
	require.Equal(t, "aW1n", cur.Get("userInputMessage.images.0.source.bytes").String())
}
