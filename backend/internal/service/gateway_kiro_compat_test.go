//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newKiroCompatContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, rec
}

func kiroHelloResponse() *http.Response {
	return kiroEventStreamResponse(
		kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`),
		kiroEventFrame("assistantResponseEvent", `{"content":" world"}`),
		kiroEventFrame("contextUsageEvent", `{"contextUsagePercentage":1}`),
	)
}

func TestGatewayService_ForwardAsChatCompletionsKiroStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/chat/completions")

	body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.NoError(t, err)

	require.Equal(t, "https://q.us-east-1.amazonaws.com/generateAssistantResponse", upstream.lastReq.URL.String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "conversationState.currentMessage.userInputMessage.content").String(), "hi")

	out := rec.Body.String()
	require.Contains(t, out, `"object":"chat.completion.chunk"`)
	require.Contains(t, out, "Hello")
	require.Contains(t, out, " world")
	require.Contains(t, out, "data: [DONE]")

	require.Equal(t, "kiro-rid", result.RequestID)
	require.Equal(t, "claude-sonnet-4-5", result.Model)
	require.NotEmpty(t, result.UpstreamModel)
	require.Positive(t, result.Usage.InputTokens)
	require.Positive(t, result.Usage.OutputTokens)
}

func TestGatewayService_ForwardAsChatCompletionsKiroNonStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/chat/completions")

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "chat.completion", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "Hello world", gjson.Get(rec.Body.String(), "choices.0.message.content").String())
	require.Equal(t, "stop", gjson.Get(rec.Body.String(), "choices.0.finish_reason").String())
	require.Positive(t, result.Usage.OutputTokens)
}

func TestGatewayService_ForwardAsChatCompletionsKiroEmptyResponseFailsOver(t *testing.T) {
	for _, stream := range []bool{true, false} {
		upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
			kiroEventFrame("contextUsageEvent", `{"contextUsagePercentage":1}`),
		)}
		svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
		c, rec := newKiroCompatContext("/v1/chat/completions")

		body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), []byte(body), nil)
		var failover *UpstreamFailoverError
		require.True(t, errors.As(err, &failover), "stream=%v", stream)
		require.True(t, failover.RetryableOnSameAccount)
		require.Equal(t, kiroEmptyResponseReason, failover.Reason)
		require.Zero(t, rec.Body.Len(), "nothing is written before failover")
	}
}

func TestGatewayService_ForwardAsChatCompletionsKiroStreamErrorAfterOutput(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
		kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`),
		kiroExceptionFrame("InternalServerException", `{"message":"boom"}`),
	)}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/chat/completions")

	body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "output already reached the client, so no failover")
	require.Contains(t, rec.Body.String(), "Hello", "content before the error reaches the client")
	require.NotNil(t, result, "content the client received is still billed")
	require.Positive(t, result.Usage.OutputTokens)
	require.Positive(t, result.Usage.InputTokens)
	require.NotNil(t, result.FirstTokenMs, "time to first token is recorded for partial responses too")
}

// kiroCanceledBody 先返回 data，再像请求被取消那样返回 context.Canceled。
type kiroCanceledBody struct{ data *bytes.Reader }

func (b *kiroCanceledBody) Read(p []byte) (int, error) {
	if b.data.Len() > 0 {
		return b.data.Read(p)
	}
	return 0, context.Canceled
}

func (b *kiroCanceledBody) Close() error { return nil }

func TestGatewayService_ForwardKiroCompatClientGoneBillsOutput(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		resp := kiroEventStreamResponse()
		resp.Body = &kiroCanceledBody{data: bytes.NewReader(
			kiroEventFrame("assistantResponseEvent", `{"content":"Hello world, this is partial output"}`))}
		svc := newKiroGatewayForTest(&kiroHTTPUpstreamStub{resp: resp}, &rateLimitAccountRepoStub{})
		c, rec := newKiroCompatContext(path)

		var (
			result *ForwardResult
			err    error
		)
		if path == "/v1/responses" {
			result, err = svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(),
				[]byte(`{"model":"claude-sonnet-4-5","stream":true,"input":"hi"}`), nil)
		} else {
			result, err = svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(),
				[]byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
		}
		require.Contains(t, rec.Body.String(), "partial output", path)
		require.NotNil(t, result, "%s: err=%v", path, err)
		require.Positive(t, result.Usage.OutputTokens, "%s: output sent before the request was canceled is billed", path)
		require.Positive(t, result.Usage.InputTokens, path)
	}
}

func TestGatewayService_ForwardAsChatCompletionsKiroContextLimit(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroJSONResponse(http.StatusBadRequest,
		`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/chat/completions")

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.True(t, strings.HasPrefix(gjson.Get(rec.Body.String(), "error.message").String(), "prompt is too long"))
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
}

func TestGatewayService_ForwardAsResponsesKiroStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/responses")

	body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"input":"hi"}`)
	result, err := svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.NoError(t, err)

	out := rec.Body.String()
	require.Contains(t, out, "response.output_text.delta")
	require.Contains(t, out, "Hello")
	require.Contains(t, out, "response.completed")
	require.Equal(t, "kiro-rid", result.RequestID)
	require.Positive(t, result.Usage.OutputTokens)
}

func TestGatewayService_ForwardAsResponsesKiroNonStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
	c, rec := newKiroCompatContext("/v1/responses")

	body := []byte(`{"model":"claude-sonnet-4-5","input":[{"role":"user","content":"hi"}]}`)
	_, err := svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(), body, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "status").String())
	require.Contains(t, rec.Body.String(), "Hello world")
}
