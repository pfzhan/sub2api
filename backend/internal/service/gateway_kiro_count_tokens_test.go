//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Kiro 没有 count_tokens 接口，本地估算：正常 body 返回与 Anthropic 相同的
// {"input_tokens":N}；解析不了的 body 返回 Anthropic 格式的 400，不 panic、不发上游请求。
func TestForwardCountTokensKiro(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		nilParsed  bool
		wantStatus int
	}{
		{name: "valid", body: `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello there"}]}`, wantStatus: http.StatusOK},
		{name: "nil_request", nilParsed: true, wantStatus: http.StatusBadRequest},
		{name: "not_an_object", body: `[]`, wantStatus: http.StatusBadRequest},
		{name: "missing_model", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantStatus: http.StatusBadRequest},
		{name: "null_message", body: `{"model":"claude-sonnet-4-5","messages":[null]}`, wantStatus: http.StatusBadRequest},
		{name: "bad_system_type", body: `{"model":"claude-sonnet-4-5","system":123,"messages":[{"role":"user","content":"hi"}]}`, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
			upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
			svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
			var parsed *ParsedRequest
			if !tc.nilParsed {
				parsed = &ParsedRequest{Body: NewRequestBodyRef([]byte(tc.body))}
			}

			var err error
			require.NotPanics(t, func() {
				err = svc.ForwardCountTokens(context.Background(), c, newKiroAccountForTest(), parsed)
			})

			require.Equal(t, tc.wantStatus, rec.Code)
			require.Nil(t, upstream.lastReq, "count_tokens is estimated locally")
			out := rec.Body.Bytes()
			if tc.wantStatus == http.StatusOK {
				require.NoError(t, err)
				require.Positive(t, gjson.GetBytes(out, "input_tokens").Int())
				require.Len(t, gjson.ParseBytes(out).Map(), 1, "response carries only input_tokens like Anthropic")
				return
			}
			require.Error(t, err)
			require.Equal(t, "error", gjson.GetBytes(out, "type").String())
			require.Equal(t, "invalid_request_error", gjson.GetBytes(out, "error.type").String())
		})
	}
}
