package kiro

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type convResult struct {
	events []SSEEvent
	conv   *Converter
	err    error
}

func runConverter(opts ConverterOptions, events ...Event) convResult {
	if opts.Model == "" {
		opts.Model = "claude-haiku-4-5"
	}
	if opts.MessageID == "" {
		opts.MessageID = "msg_test"
	}
	c := NewConverter(opts)
	var out []SSEEvent
	for _, ev := range events {
		out = append(out, c.Handle(ev)...)
	}
	tail, err := c.Finish()
	return convResult{events: append(out, tail...), conv: c, err: err}
}

func (r convResult) names() []string {
	names := make([]string, len(r.events))
	for i, e := range r.events {
		names[i] = e.Event
		if e.Event == "content_block_start" {
			names[i] += ":" + gjson.GetBytes(e.Data, "content_block.type").String()
		}
	}
	return names
}

func (r convResult) deltas(kind string) string {
	var sb strings.Builder
	for _, e := range r.events {
		if e.Event != "content_block_delta" {
			continue
		}
		d := gjson.GetBytes(e.Data, "delta")
		if d.Get("type").String() == kind {
			sb.WriteString(d.Get("text").String() + d.Get("thinking").String() + d.Get("partial_json").String())
		}
	}
	return sb.String()
}

func text(s string) Event { return Event{Kind: EventText, Text: s} }

func TestConverterPlainText(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{EstimatedInputTokens: 42}, text("Hel"), text("lo"), text("lo"))
	require.NoError(t, r.err)
	require.Equal(t, []string{
		"message_start", "content_block_start:text", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}, r.names())
	require.Equal(t, "Hello", r.deltas("text_delta"), "consecutive duplicate chunk is dropped")
	require.Equal(t, "end_turn", r.conv.StopReason())
	require.Equal(t, 42, r.conv.Usage().InputTokens)
	require.Equal(t, int64(42), gjson.GetBytes(r.events[0].Data, "message.usage.input_tokens").Int())

	msg := gjson.ParseBytes(r.conv.Message())
	require.Equal(t, "msg_test", msg.Get("id").String())
	require.Equal(t, "Hello", msg.Get("content.0.text").String())
	require.Equal(t, "end_turn", msg.Get("stop_reason").String())
}

func TestConverterEmptyResponse(t *testing.T) {
	t.Parallel()
	c := NewConverter(ConverterOptions{})
	require.Empty(t, c.Handle(Event{Kind: EventContextUsage, ContextUsagePercentage: 2}))
	events, err := c.Finish()
	require.ErrorIs(t, err, ErrEmptyResponse)
	require.Empty(t, events)
	require.False(t, c.Started())
}

func TestConverterThinkingTagsAcrossChunks(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{ThinkingRequested: true},
		text("\n<thin"), text("king>\nLet me think about `</thinking>` here"), text(".</thi"), text("nking>\n"), text("\nAnswer"))
	require.NoError(t, r.err)
	require.Equal(t, []string{
		"message_start", "content_block_start:thinking", "content_block_delta", "content_block_delta", "content_block_delta",
		"content_block_stop", "content_block_start:text", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}, r.names())
	require.Equal(t, "Let me think about `</thinking>` here.", r.deltas("thinking_delta"))
	require.Equal(t, "Answer", r.deltas("text_delta"))

	msg := gjson.ParseBytes(r.conv.Message())
	require.Equal(t, "thinking", msg.Get("content.0.type").String())
	require.Equal(t, "Answer", msg.Get("content.1.text").String())
}

func TestConverterThinkingNotRequestedKeepsTags(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{}, text("<thinking>x</thinking>\n\ny"))
	require.Equal(t, "<thinking>x</thinking>\n\ny", r.deltas("text_delta"))
}

func TestConverterThinkingOnly(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{ThinkingRequested: true}, Event{Kind: EventReasoning, Text: "deep"})
	require.NoError(t, r.err)
	require.Equal(t, "max_tokens", r.conv.StopReason())
	require.Equal(t, " ", r.deltas("text_delta"))
	require.Equal(t, "deep", r.deltas("thinking_delta"))
}

func TestConverterToolUse(t *testing.T) {
	t.Parallel()
	long := "mcp__" + strings.Repeat("x", 80)
	names := newToolNames([]claudeTool{{Name: long}})
	short := names.Upstream(long)
	r := runConverter(ConverterOptions{Tools: names, UpstreamModel: "claude-opus-4.7"},
		text("Let me check."),
		Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: short, ToolInput: `{"path":`},
		Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: short, ToolInput: `"a"}`},
		Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: short, ToolStop: true},
		Event{Kind: EventToolUse, ToolUseID: "p1", ToolName: PlaceholderToolName, ToolInput: "{}"},
		Event{Kind: EventToolUse, ToolUseID: "p1", ToolInput: "", ToolStop: true},
		Event{Kind: EventContextUsage, ContextUsagePercentage: 1},
	)
	require.NoError(t, r.err)
	require.Equal(t, []string{
		"message_start", "content_block_start:text", "content_block_delta",
		"content_block_stop", "content_block_start:tool_use", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}, r.names())
	require.Equal(t, long, gjson.GetBytes(r.events[4].Data, "content_block.name").String())
	require.Equal(t, `{"path":"a"}`, r.deltas("input_json_delta"))
	require.Equal(t, "tool_use", r.conv.StopReason())

	usage := r.conv.Usage()
	require.Positive(t, usage.OutputTokens)
	require.Equal(t, 10_000-usage.OutputTokens, usage.InputTokens)

	msg := gjson.ParseBytes(r.conv.Message())
	require.Equal(t, "a", msg.Get("content.1.input.path").String())
	require.Equal(t, "t1", msg.Get("content.1.id").String())
}

func TestConverterOnlyPlaceholderToolIsEmpty(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{},
		Event{Kind: EventToolUse, ToolUseID: "p1", ToolName: PlaceholderToolName, ToolInput: "{}", ToolStop: true})
	require.ErrorIs(t, r.err, ErrEmptyResponse)
}

func TestConverterUnterminatedToolIsClosed(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{}, Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: "read", ToolInput: `{"a":`})
	require.NoError(t, r.err)
	require.Equal(t, "tool_use", r.conv.StopReason())
	msg := gjson.ParseBytes(r.conv.Message())
	require.Equal(t, "{}", msg.Get("content.0.input").Raw, "invalid JSON input falls back to an empty object")
}

func TestConverterKeepsUTF8Boundaries(t *testing.T) {
	t.Parallel()
	r := runConverter(ConverterOptions{ThinkingRequested: true}, text("你好世界，这是一段较长的中文"), text("回答"))
	require.NoError(t, r.err)
	require.Equal(t, "你好世界，这是一段较长的中文回答", r.deltas("text_delta"))
}

func TestSSEEventAppendTo(t *testing.T) {
	t.Parallel()
	e := SSEEvent{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)}
	require.Equal(t, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", string(e.AppendTo(nil)))
}
