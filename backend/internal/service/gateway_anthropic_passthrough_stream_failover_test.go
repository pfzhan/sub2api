package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func passthroughStreamFailoverService(resp *http.Response) (*GatewayService, *gatewayForwardErrorPolicyRepoStub) {
	cfg := &config.Config{
		Gateway:  config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}},
	}
	repo := &gatewayForwardErrorPolicyRepoStub{}
	return &GatewayService{
		cfg:              cfg,
		httpUpstream:     &anthropicHTTPUpstreamRecorder{resp: resp},
		rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil),
	}, repo
}

func passthroughStreamResponse(body *streamReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"req_1"}},
		Body:       body,
	}
}

func passthroughStreamContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

// 与 /v1/messages 一致：输出开始前的上游读错误交给 handler 换号，客户端看不到任何字节，
// 错误消息不带网络地址。
func TestAnthropicAPIKeyPassthrough_ReadErrorBeforeOutputFailsOver(t *testing.T) {
	c, rec := passthroughStreamContext()
	readErr := &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 54321},
		Addr:   &net.TCPAddr{IP: net.ParseIP("52.1.2.3"), Port: 443},
		Err:    errors.New("connection reset by peer"),
	}
	svc, _ := passthroughStreamFailoverService(passthroughStreamResponse(&streamReadCloser{err: readErr}))

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-6","stream":true}`), "claude-sonnet-4-6", "claude-sonnet-4-6", true, time.Now())

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.NotContains(t, string(failoverErr.ResponseBody), "10.0.0.1")
	require.NotContains(t, string(failoverErr.ResponseBody), "52.1.2.3")
	require.Zero(t, rec.Body.Len(), "nothing may reach the client before failover")
}

// 与 /v1/messages 一致：输出开始前的 event:error 帧不透传给客户端，按上游错误换号。
func TestAnthropicAPIKeyPassthrough_ErrorEventBeforeOutputFailsOver(t *testing.T) {
	for _, frame := range []string{
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
		"event:error\r\ndata:{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\r\n\r\n",
		// 流在 error 事件的空行之前结束
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n",
	} {
		c, rec := passthroughStreamContext()
		svc, repo := passthroughStreamFailoverService(passthroughStreamResponse(&streamReadCloser{payload: []byte(frame)}))

		result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-6","stream":true}`), "claude-sonnet-4-6", "claude-sonnet-4-6", true, time.Now())

		require.Nil(t, result)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr, "frame %q", frame)
		require.Equal(t, 529, failoverErr.StatusCode)
		require.Contains(t, string(failoverErr.ResponseBody), "overloaded_error")
		require.Zero(t, rec.Body.Len(), "the upstream error frame must not reach the client before failover")
		require.Equal(t, 1, repo.overloadCalls, "an overloaded frame before output marks the account like an HTTP 529")
	}
}

// 已开始输出后的 event:error 仍按透传语义原样送达客户端，不换号。
func TestAnthropicAPIKeyPassthrough_ErrorEventAfterOutputIsPassedThrough(t *testing.T) {
	c, rec := passthroughStreamContext()
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	svc, repo := passthroughStreamFailoverService(passthroughStreamResponse(&streamReadCloser{payload: []byte(stream)}))

	_, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-6","stream":true}`), "claude-sonnet-4-6", "claude-sonnet-4-6", true, time.Now())

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "output already started, the handler must not switch accounts")
	require.True(t, strings.Contains(rec.Body.String(), "event: error"))
	require.True(t, strings.Contains(rec.Body.String(), "overloaded_error"))
	require.Zero(t, repo.overloadCalls)
}
