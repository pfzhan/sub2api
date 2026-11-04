//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCountTokensRetryFixture(responses ...*http.Response) (*GatewayService, *queuedHTTPUpstream, *tempUnschedScopeRepoStub, *Account) {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	repo := &tempUnschedScopeRepoStub{}
	upstream := &queuedHTTPUpstream{responses: responses}
	svc := &GatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{accountRepo: repo, cfg: cfg},
	}
	account := &Account{
		ID:          501,
		Name:        "count-tokens-403-retry",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-oauth-token"},
		Status:      StatusActive,
		Schedulable: true,
	}
	return svc, upstream, repo, account
}

func newCountTokensRetryContext() (*gin.Context, *httptest.ResponseRecorder, *ParsedRequest) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	return c, rec, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-5"}
}

func TestForwardCountTokens_OAuth403RetriesBeforeMarkingAccount(t *testing.T) {
	svc, upstream, repo, account := newCountTokensRetryFixture(
		newJSONResponse(http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"transient"}}`),
		newJSONResponse(http.StatusOK, `{"input_tokens":7}`),
	)
	c, rec, parsed := newCountTokensRetryContext()

	require.NoError(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))

	require.Len(t, upstream.requests, 2, "OAuth 403 is retried on the same account, as on Messages")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"input_tokens":7}`, rec.Body.String())
	require.Zero(t, repo.errorCalls, "a 403 that recovers on retry must not disable the account")
}

func TestForwardCountTokens_OAuth403MarksAccountAfterRetriesExhausted(t *testing.T) {
	responses := make([]*http.Response, 0, maxRetryAttempts)
	for i := 0; i < maxRetryAttempts; i++ {
		responses = append(responses, newJSONResponse(http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"forbidden"}}`))
	}
	svc, upstream, repo, account := newCountTokensRetryFixture(responses...)
	c, rec, parsed := newCountTokensRetryContext()

	require.Error(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))

	require.Len(t, upstream.requests, maxRetryAttempts)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, 1, repo.errorCalls, "the account is marked once, after retries are exhausted")
}

func TestForwardCountTokens_OAuth403StopsRetryWhenClientCancels(t *testing.T) {
	svc, upstream, repo, account := newCountTokensRetryFixture(
		newJSONResponse(http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"transient"}}`),
		newJSONResponse(http.StatusOK, `{"input_tokens":7}`),
	)
	c, _, parsed := newCountTokensRetryContext()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, svc.ForwardCountTokens(ctx, c, account, parsed), context.Canceled)

	require.Len(t, upstream.requests, 1)
	require.Zero(t, repo.errorCalls, "a client that leaves during backoff must not cause the account to be marked")
}
