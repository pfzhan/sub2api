//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// bedrockChunkFrame 构造一个携带 Claude SSE 事件的 Bedrock EventStream chunk 帧。
func bedrockChunkFrame(t *testing.T, event string) []byte {
	t.Helper()
	payload := []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(event)) + `"}`)
	var headers bytes.Buffer
	for _, h := range [][2]string{{":event-type", "chunk"}, {":message-type", "event"}} {
		_ = headers.WriteByte(byte(len(h[0])))
		_, _ = headers.WriteString(h[0])
		_ = headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(h[1])))
		_, _ = headers.WriteString(h[1])
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(12+headers.Len()+len(payload)+4))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	_, _ = frame.Write(headers.Bytes())
	_, _ = frame.Write(payload)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}

func TestGatewayService_ForwardBedrock_StreamReadErrorAfterOutputPreservesPartialUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{"model":"claude-sonnet-5-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	frame := bedrockChunkFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":9,"cache_read_input_tokens":3}}}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Amzn-Requestid": []string{"aws-rid"}},
		Body:       io.NopCloser(io.MultiReader(bytes.NewReader(frame), iotest.ErrReader(io.ErrUnexpectedEOF))),
	}}
	svc := newForwardPartialUsageServiceForTest(upstream)
	account := &Account{
		ID: 601, Name: "bedrock-partial-usage", Platform: PlatformAnthropic, Type: AccountTypeBedrock,
		Concurrency: 1, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"auth_mode": "apikey", "api_key": "test-key", "aws_region": "us-east-1"},
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)

	require.ErrorContains(t, err, "bedrock stream read error")
	require.Contains(t, rec.Body.String(), "message_start", "output reached the client before the read error")
	require.NotNil(t, result, "usage observed before the read error must be returned for billing")
	require.True(t, result.Stream)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.CacheReadInputTokens)
	require.Equal(t, "aws-rid", result.RequestID)
	require.Equal(t, "claude-sonnet-5-5", result.Model)
}
