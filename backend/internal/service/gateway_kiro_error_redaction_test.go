//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Kiro 上游 HTTP 错误里的 ARN 不能出现在任何对外内容里：不换账号的错误直接写给
// 客户端，换账号的错误体会在 failover 用尽时经透传规则对外输出。
func TestKiroUpstreamHTTPErrorRedactsARNOnEveryEntry(t *testing.T) {
	entries := map[string]func(svc *GatewayService) (string, error){
		"messages": func(svc *GatewayService) (string, error) {
			_, rec, err := forwardKiroForTest(t, svc,
				`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
			return rec.Body.String(), err
		},
		"chat_completions": func(svc *GatewayService) (string, error) {
			c, rec := newKiroCompatContext("/v1/chat/completions")
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(),
				[]byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`), nil)
			return rec.Body.String(), err
		},
		"responses": func(svc *GatewayService) (string, error) {
			c, rec := newKiroCompatContext("/v1/responses")
			_, err := svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(),
				[]byte(`{"model":"claude-sonnet-4-5","input":"hi"}`), nil)
			return rec.Body.String(), err
		},
	}

	for name, forward := range entries {
		t.Run(name+"/client_error", func(t *testing.T) {
			upstream := &kiroHTTPUpstreamStub{resp: kiroJSONResponse(http.StatusBadRequest,
				`{"message":"Validation failed for `+kiroTestARN+`"}`)}
			svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

			out, err := forward(svc)
			require.Error(t, err)
			require.Contains(t, out, "Validation failed")
			require.NotContains(t, out, "699475941385")
			require.NotContains(t, err.Error(), "699475941385")
		})

		for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
			t.Run(name+"/failover_"+http.StatusText(status), func(t *testing.T) {
				upstream := &kiroHTTPUpstreamStub{resp: kiroJSONResponse(status,
					`{"message":"Denied for `+kiroTestARN+`"}`)}
				svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

				out, err := forward(svc)
				var failover *UpstreamFailoverError
				require.True(t, errors.As(err, &failover), "status %d must fail over", status)
				require.Empty(t, out, "failover must not write to the client")
				require.Contains(t, gjson.GetBytes(failover.ResponseBody, "error.message").String(), "Denied for")
				require.NotContains(t, string(failover.ResponseBody), "699475941385")
			})
		}
	}
}
