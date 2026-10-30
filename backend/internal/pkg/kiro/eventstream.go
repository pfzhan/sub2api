package kiro

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

// AWS event stream 帧格式：
//
//	[u32 total length][u32 headers length][u32 prelude crc][headers][payload][u32 message crc]
//
// 两个 CRC 都是 CRC32（IEEE）。
const (
	preludeLen     = 12
	messageCRCLen  = 4
	minFrameLen    = preludeLen + messageCRCLen
	maxFrameLen    = 16 << 20
	readBufferSize = 64 << 10
)

// Frame 是一个解码后的 event stream 消息。
type Frame struct {
	Headers map[string]string
	Payload []byte
}

// Decoder 从 HTTP 响应体中逐帧读取 event stream。遇到长度或 CRC 不合法的
// 帧头时丢弃一个字节重新对齐，与 AWS SDK 遇到损坏数据时的处理一致。
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder 创建解码器。
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, readBufferSize)}
}

// Next 返回下一帧。流正常结束时返回 io.EOF；流在帧中间截断时返回
// io.ErrUnexpectedEOF。
func (d *Decoder) Next() (*Frame, error) {
	for {
		prelude, err := d.r.Peek(preludeLen)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(prelude) == 0 {
					return nil, io.EOF
				}
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		total := binary.BigEndian.Uint32(prelude[0:4])
		headersLen := binary.BigEndian.Uint32(prelude[4:8])
		preludeCRC := binary.BigEndian.Uint32(prelude[8:12])
		if total < minFrameLen || total > maxFrameLen || headersLen > total-minFrameLen ||
			crc32.ChecksumIEEE(prelude[:8]) != preludeCRC {
			if _, err := d.r.Discard(1); err != nil {
				return nil, err
			}
			continue
		}

		frame := make([]byte, total)
		if _, err := io.ReadFull(d.r, frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		msgCRC := binary.BigEndian.Uint32(frame[total-messageCRCLen:])
		if crc32.ChecksumIEEE(frame[:total-messageCRCLen]) != msgCRC {
			continue
		}
		headers, err := parseHeaders(frame[preludeLen : preludeLen+headersLen])
		if err != nil {
			continue
		}
		return &Frame{Headers: headers, Payload: frame[preludeLen+headersLen : total-messageCRCLen]}, nil
	}
}

// parseHeaders 解析帧头，只保留字符串类型的值，其余类型跳过。
func parseHeaders(b []byte) (map[string]string, error) {
	headers := make(map[string]string, 4)
	for len(b) > 0 {
		nameLen := int(b[0])
		if len(b) < 1+nameLen+1 {
			return nil, errors.New("truncated header name")
		}
		name := string(b[1 : 1+nameLen])
		valueType := b[1+nameLen]
		b = b[2+nameLen:]

		var size int
		switch valueType {
		case 0, 1: // bool true / false
			size = 0
		case 2: // byte
			size = 1
		case 3: // short
			size = 2
		case 4: // int
			size = 4
		case 5, 8: // long, timestamp
			size = 8
		case 9: // uuid
			size = 16
		case 6, 7: // bytes, string
			if len(b) < 2 {
				return nil, errors.New("truncated header value length")
			}
			n := int(binary.BigEndian.Uint16(b))
			if len(b) < 2+n {
				return nil, errors.New("truncated header value")
			}
			if valueType == 7 {
				headers[name] = string(b[2 : 2+n])
			}
			b = b[2+n:]
			continue
		default:
			return nil, fmt.Errorf("unknown header value type %d", valueType)
		}
		if len(b) < size {
			return nil, errors.New("truncated header value")
		}
		b = b[size:]
	}
	return headers, nil
}

// EventKind 是 Kiro 流事件的类别。
type EventKind int

const (
	EventUnknown EventKind = iota
	// EventText 是正文增量（assistantResponseEvent）。
	EventText
	// EventReasoning 是原生思考增量（reasoningContentEvent）。
	EventReasoning
	// EventToolUse 是工具调用的开始、参数增量或结束（toolUseEvent）。
	EventToolUse
	// EventContextUsage 报告本轮上下文占用百分比，用于换算输入 token。
	EventContextUsage
	// EventException 是上游在流内返回的异常。
	EventException
)

// Event 是解析后的 Kiro 流事件。
type Event struct {
	Kind EventKind
	// Text 用于 EventText 与 EventReasoning；EventException 时为错误信息。
	Text string

	ToolUseID string
	ToolName  string
	// ToolInput 是参数 JSON 的一段增量。
	ToolInput string
	ToolStop  bool

	ContextUsagePercentage float64

	// ExceptionType 是 :exception-type 或 :error-code 的值。
	ExceptionType string
}

// ParseEvent 把一帧转换为事件。无关事件（followupPrompt、metering、
// 签名等）返回 EventUnknown。
func ParseEvent(frame *Frame) Event {
	if frame == nil {
		return Event{}
	}
	switch frame.Headers[":message-type"] {
	case "exception", "error":
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frame.Payload, &body)
		return Event{
			Kind:          EventException,
			Text:          firstNonEmpty(body.Message, frame.Headers[":error-message"], string(frame.Payload)),
			ExceptionType: firstNonEmpty(frame.Headers[":exception-type"], frame.Headers[":error-code"]),
		}
	}

	var p struct {
		Content                *string         `json:"content"`
		FollowupPrompt         json.RawMessage `json:"followupPrompt"`
		Text                   *string         `json:"text"`
		Name                   string          `json:"name"`
		ToolUseID              string          `json:"toolUseId"`
		Input                  json.RawMessage `json:"input"`
		Stop                   *bool           `json:"stop"`
		ContextUsagePercentage *float64        `json:"contextUsagePercentage"`
	}
	if err := json.Unmarshal(frame.Payload, &p); err != nil {
		return Event{}
	}
	text := func(kind EventKind, s *string) Event {
		if s == nil {
			return Event{}
		}
		return Event{Kind: kind, Text: *s}
	}
	toolUse := func() Event {
		if p.ToolUseID == "" && p.Input == nil && p.Stop == nil {
			return Event{}
		}
		return Event{
			Kind:      EventToolUse,
			ToolUseID: p.ToolUseID,
			ToolName:  p.Name,
			ToolInput: rawToolInput(p.Input),
			ToolStop:  p.Stop != nil && *p.Stop,
		}
	}
	hasFollowup := len(p.FollowupPrompt) > 0 && string(p.FollowupPrompt) != "null"

	switch frame.Headers[":event-type"] {
	case "assistantResponseEvent":
		if hasFollowup {
			return Event{}
		}
		return text(EventText, p.Content)
	case "reasoningContentEvent":
		return text(EventReasoning, p.Text)
	case "toolUseEvent":
		return toolUse()
	case "contextUsageEvent":
		if p.ContextUsagePercentage != nil {
			return Event{Kind: EventContextUsage, ContextUsagePercentage: *p.ContextUsagePercentage}
		}
		return Event{}
	}

	// 没有 :event-type 或类型未知时按负载字段判断。
	switch {
	case p.ContextUsagePercentage != nil:
		return Event{Kind: EventContextUsage, ContextUsagePercentage: *p.ContextUsagePercentage}
	case p.Content != nil:
		if hasFollowup {
			return Event{}
		}
		return text(EventText, p.Content)
	case p.Text != nil:
		return text(EventReasoning, p.Text)
	case p.Name != "" || p.ToolUseID != "" || p.Input != nil || p.Stop != nil:
		return toolUse()
	}
	return Event{}
}

// rawToolInput 把 input 字段统一为字符串。Kiro 通常以字符串分段发送 JSON，
// 个别情况下直接给出对象。
func rawToolInput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return strings.TrimSpace(string(raw))
}
