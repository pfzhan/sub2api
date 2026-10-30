package kiro

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// encodeFrame 按 AWS event stream 格式编码一帧，头部值均为字符串。
func encodeFrame(headers [][2]string, payload string) []byte {
	var hb bytes.Buffer
	for _, h := range headers {
		hb.WriteByte(byte(len(h[0])))
		hb.WriteString(h[0])
		hb.WriteByte(7)
		_ = binary.Write(&hb, binary.BigEndian, uint16(len(h[1])))
		hb.WriteString(h[1])
	}
	total := preludeLen + hb.Len() + len(payload) + messageCRCLen
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(total))
	_ = binary.Write(&buf, binary.BigEndian, uint32(hb.Len()))
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(buf.Bytes()[:8]))
	buf.Write(hb.Bytes())
	buf.WriteString(payload)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(buf.Bytes()))
	return buf.Bytes()
}

func eventFrame(eventType, payload string) []byte {
	return encodeFrame([][2]string{
		{":message-type", "event"},
		{":event-type", eventType},
		{":content-type", "application/json"},
	}, payload)
}

func decodeAll(t *testing.T, data []byte) []Event {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	var events []Event
	for {
		f, err := d.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		require.NoError(t, err)
		events = append(events, ParseEvent(f))
	}
}

func TestDecoderParsesEvents(t *testing.T) {
	t.Parallel()
	var stream []byte
	stream = append(stream, eventFrame("assistantResponseEvent", `{"content":"Hello"}`)...)
	stream = append(stream, eventFrame("assistantResponseEvent", `{"content":"x","followupPrompt":{"content":"y"}}`)...)
	stream = append(stream, eventFrame("reasoningContentEvent", `{"text":"hmm"}`)...)
	stream = append(stream, eventFrame("toolUseEvent", `{"name":"read","toolUseId":"t1","input":"{\"a\""}`)...)
	stream = append(stream, eventFrame("toolUseEvent", `{"name":"read","toolUseId":"t1","input":":1}","stop":true}`)...)
	stream = append(stream, eventFrame("contextUsageEvent", `{"contextUsagePercentage":1.5}`)...)
	stream = append(stream, eventFrame("meteringEvent", `{"unit":"credit","usage":0.1}`)...)

	events := decodeAll(t, stream)
	require.Len(t, events, 7)
	require.Equal(t, Event{Kind: EventText, Text: "Hello"}, events[0])
	require.Equal(t, EventUnknown, events[1].Kind)
	require.Equal(t, Event{Kind: EventReasoning, Text: "hmm"}, events[2])
	require.Equal(t, Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: "read", ToolInput: `{"a"`}, events[3])
	require.Equal(t, Event{Kind: EventToolUse, ToolUseID: "t1", ToolName: "read", ToolInput: ":1}", ToolStop: true}, events[4])
	require.Equal(t, Event{Kind: EventContextUsage, ContextUsagePercentage: 1.5}, events[5])
	require.Equal(t, EventUnknown, events[6].Kind)
}

func TestDecoderResyncsAfterGarbage(t *testing.T) {
	t.Parallel()
	stream := append([]byte{0, 0, 0, 20, 0xff, 0xff}, eventFrame("assistantResponseEvent", `{"content":"ok"}`)...)
	events := decodeAll(t, stream)
	require.Equal(t, []Event{{Kind: EventText, Text: "ok"}}, events)
}

func TestDecoderSkipsCorruptedMessage(t *testing.T) {
	t.Parallel()
	bad := eventFrame("assistantResponseEvent", `{"content":"bad"}`)
	bad[len(bad)-6] ^= 0xff
	stream := append(bad, eventFrame("assistantResponseEvent", `{"content":"good"}`)...)
	events := decodeAll(t, stream)
	require.Equal(t, []Event{{Kind: EventText, Text: "good"}}, events)
}

func TestDecoderTruncatedFrame(t *testing.T) {
	t.Parallel()
	frame := eventFrame("assistantResponseEvent", `{"content":"Hello"}`)
	d := NewDecoder(bytes.NewReader(frame[:len(frame)-3]))
	_, err := d.Next()
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestParseEventException(t *testing.T) {
	t.Parallel()
	frame := encodeFrame([][2]string{
		{":message-type", "exception"},
		{":exception-type", "ThrottlingException"},
	}, `{"message":"slow down"}`)
	events := decodeAll(t, frame)
	require.Equal(t, []Event{{Kind: EventException, Text: "slow down", ExceptionType: "ThrottlingException"}}, events)
}

func TestParseEventWithoutEventType(t *testing.T) {
	t.Parallel()
	frame := encodeFrame(nil, `{"contextUsagePercentage":3}`)
	events := decodeAll(t, frame)
	require.Equal(t, []Event{{Kind: EventContextUsage, ContextUsagePercentage: 3}}, events)
}
