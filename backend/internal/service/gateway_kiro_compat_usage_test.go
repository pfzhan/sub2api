//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const kiroTestARN = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"

// kiroCanceledAfter 先返回给定帧，再返回 context.Canceled，模拟客户端断开后
// 上游请求被取消。
func kiroCanceledAfter(frames ...[]byte) *http.Response {
	resp := kiroEventStreamResponse(frames...)
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bytes.Join(frames, nil)), kiroErrReader{context.Canceled}))
	return resp
}

type kiroErrReader struct{ err error }

func (r kiroErrReader) Read([]byte) (int, error) { return 0, r.err }

func kiroCompatForward(svc *GatewayService, endpoint string, stream bool) (*ForwardResult, string, error) {
	path, body := "/v1/chat/completions", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]`
	if endpoint == "responses" {
		path, body = "/v1/responses", `{"model":"claude-sonnet-4-5","input":"hi"`
	}
	if stream {
		body += `,"stream":true`
	}
	body += "}"
	c, rec := newKiroCompatContext(path)
	var (
		result *ForwardResult
		err    error
	)
	if endpoint == "responses" {
		result, err = svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(), []byte(body), nil)
	} else {
		result, err = svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), []byte(body), nil)
	}
	return result, rec.Body.String(), err
}

func TestKiroCompatStreamErrorAfterOutputBillsSettledUsage(t *testing.T) {
	for _, endpoint := range []string{"chat_completions", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
				kiroEventFrame("assistantResponseEvent", `{"content":"Hello there"}`),
				kiroExceptionFrame("InternalServerException", `{"message":"boom for `+kiroTestARN+`"}`),
			)}
			svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

			result, out, err := kiroCompatForward(svc, endpoint, true)

			require.Error(t, err)
			require.Contains(t, out, "Hello there")
			require.NotNil(t, result, "output reached the client, so the produced content is billed")
			require.Positive(t, result.Usage.InputTokens)
			require.Positive(t, result.Usage.OutputTokens, "output tokens come from the settled converter")
			require.Equal(t, "kiro-rid", result.RequestID)
			require.NotContains(t, out, "699475941385")
		})
	}
}

func TestKiroCompatClientDisconnectBillsSettledUsage(t *testing.T) {
	for _, endpoint := range []string{"chat_completions", "responses"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				upstream := &kiroHTTPUpstreamStub{resp: kiroCanceledAfter(
					kiroEventFrame("assistantResponseEvent", `{"content":"Hello there"}`),
				)}
				svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

				result, _, err := kiroCompatForward(svc, endpoint, stream)

				require.NoError(t, err, "stream=%v", stream)
				require.NotNil(t, result, "stream=%v", stream)
				require.True(t, result.ClientDisconnect)
				require.Positive(t, result.Usage.OutputTokens, "stream=%v", stream)
			})
		}
	}
}

func TestKiroMessagesStreamErrorRedactsUpstreamDetails(t *testing.T) {
	for name, frame := range map[string][]byte{
		"exception": kiroExceptionFrame("ValidationException", `{"message":"bad profile `+kiroTestARN+`"}`),
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
				kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`),
				frame,
			)}
			svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

			_, rec, err := forwardKiroForTest(t, svc,
				`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

			require.Error(t, err)
			out := rec.Body.String()
			require.Contains(t, out, "event: error")
			require.Contains(t, out, "ValidationException")
			require.NotContains(t, out, "699475941385")
		})
	}
}

func TestKiroStreamReadErrorHidesRawError(t *testing.T) {
	resp := kiroEventStreamResponse(kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`))
	resp.Body = io.NopCloser(io.MultiReader(
		bytes.NewReader(kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`)),
		kiroErrReader{errors.New("read tcp 10.0.0.5:51234->52.1.2.3:443: secret detail")},
	))
	svc := newKiroGatewayForTest(&kiroHTTPUpstreamStub{resp: resp}, &rateLimitAccountRepoStub{})

	_, rec, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	require.Error(t, err)
	out := rec.Body.String()
	require.Contains(t, out, "upstream stream read error")
	require.NotContains(t, out, "10.0.0.5")
	require.NotContains(t, out, "secret detail")
}
