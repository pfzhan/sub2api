//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestForwardCountTokens_VertexServiceAccountReturnsNotFoundWithoutUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-5"}
	upstream := &anthropicHTTPUpstreamRecorder{err: context.Canceled}
	svc := &GatewayService{httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	account := &Account{ID: 301, Platform: PlatformAnthropic, Type: AccountTypeServiceAccount, Status: StatusActive, Schedulable: true}

	err := svc.ForwardCountTokens(context.Background(), c, account, parsed)

	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Nil(t, upstream.lastReq, "the Google access token must not be sent to the Anthropic count_tokens endpoint")
}
