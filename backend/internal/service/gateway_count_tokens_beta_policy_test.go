package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCountTokensBetaPolicyService(t *testing.T, rules []BetaPolicyRule) (*GatewayService, *anthropicHTTPUpstreamRecorder) {
	t.Helper()
	raw, err := json.Marshal(&BetaPolicySettings{Rules: rules})
	require.NoError(t, err)
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
		},
	}
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
		settingService: NewSettingService(
			&gatewayTTLSettingRepo{data: map[string]string{SettingKeyBetaPolicySettings: string(raw)}},
			&config.Config{},
		),
	}
	return svc, upstream
}

func newCountTokensBetaPolicyContext(beta string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	c.Request.Header.Set("anthropic-beta", beta)
	return c, rec
}

func newCountTokensBetaPolicyAPIKeyAccount(mapping map[string]any) *Account {
	creds := map[string]any{"api_key": "test-key", "base_url": "https://api.anthropic.com"}
	if mapping != nil {
		creds["model_mapping"] = mapping
	}
	return &Account{
		ID:          611,
		Name:        "count-tokens-beta-policy",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: creds,
		Status:      StatusActive,
		Schedulable: true,
	}
}

// /v1/messages 在 Forward 入口按 block 规则拦截客户端 beta；count_tokens 必须拦同一批，
// 不能把被拦的 beta 发往上游。
func TestForwardCountTokens_BetaPolicyBlockMatchesMessages(t *testing.T) {
	rules := []BetaPolicyRule{{
		BetaToken:    "context-management-2025-06-27",
		Action:       BetaPolicyActionBlock,
		Scope:        BetaPolicyScopeAll,
		ErrorMessage: "context management is blocked",
	}}
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	for _, account := range []*Account{
		newCountTokensBetaPolicyAPIKeyAccount(nil),
		{
			ID: 612, Name: "count-tokens-beta-policy-oauth", Platform: PlatformAnthropic,
			Type: AccountTypeSetupToken, Concurrency: 1, Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"access_token": "test-oauth-token"},
		},
	} {
		t.Run(account.Type, func(t *testing.T) {
			// Messages 侧：同一账号同一模型被拦。
			msgSvc, _ := newCountTokensBetaPolicyService(t, rules)
			mc, _ := newCountTokensBetaPolicyContext("context-management-2025-06-27")
			_, err := msgSvc.Forward(context.Background(), mc, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-6"})
			var blocked *BetaBlockedError
			require.True(t, errors.As(err, &blocked), "messages should block, got %v", err)

			svc, upstream := newCountTokensBetaPolicyService(t, rules)
			c, rec := newCountTokensBetaPolicyContext("context-management-2025-06-27")
			err = svc.ForwardCountTokens(context.Background(), c, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-6"})
			require.Error(t, err)
			require.True(t, errors.As(err, &blocked), "count_tokens should return *BetaBlockedError, got %T", err)
			require.Nil(t, upstream.lastReq, "blocked beta must not reach upstream")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `"invalid_request_error"`)
			require.Contains(t, rec.Body.String(), "context management is blocked")
		})
	}
}

// 带模型白名单的 filter 规则按客户端请求的模型求值（与 Forward 相同），
// 账号模型映射后的名字不应让 count_tokens 与 messages 得出不同的过滤结果。
func TestForwardCountTokens_BetaPolicyFilterUsesRequestModelLikeMessages(t *testing.T) {
	rules := []BetaPolicyRule{{
		BetaToken:      "context-1m-2025-08-07",
		Action:         BetaPolicyActionPass,
		Scope:          BetaPolicyScopeAll,
		ModelWhitelist: []string{"claude-sonnet-5", "claude-sonnet-5-*"},
		FallbackAction: BetaPolicyActionFilter,
	}}
	account := newCountTokensBetaPolicyAPIKeyAccount(map[string]any{"team-sonnet": "claude-sonnet-5"})
	body := []byte(`{"model":"team-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	beta := "context-1m-2025-08-07,interleaved-thinking-2025-05-14"

	// Messages 侧的过滤集合。
	msgSvc, _ := newCountTokensBetaPolicyService(t, rules)
	mc, _ := newCountTokensBetaPolicyContext(beta)
	_, _ = msgSvc.Forward(context.Background(), mc, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "team-sonnet"})
	cached, ok := mc.Get(betaPolicyFilterSetKey)
	require.True(t, ok)
	_, msgFiltered := cached.(map[string]struct{})["context-1m-2025-08-07"]

	svc, upstream := newCountTokensBetaPolicyService(t, rules)
	c, _ := newCountTokensBetaPolicyContext(beta)
	require.NoError(t, svc.ForwardCountTokens(context.Background(), c, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: "team-sonnet"}))
	require.NotNil(t, upstream.lastReq)
	ctFiltered := !containsBetaToken(getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"), "context-1m-2025-08-07")

	require.Equal(t, msgFiltered, ctFiltered, "messages and count_tokens must filter context-1m the same way")
	require.True(t, ctFiltered)
	require.True(t, containsBetaToken(getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"), "interleaved-thinking-2025-05-14"))
}

// 没有命中 block 时 count_tokens 照常转发。
func TestForwardCountTokens_BetaPolicyPassesUnblockedBeta(t *testing.T) {
	rules := []BetaPolicyRule{{
		BetaToken: "context-management-2025-06-27",
		Action:    BetaPolicyActionBlock,
		Scope:     BetaPolicyScopeAll,
	}}
	svc, upstream := newCountTokensBetaPolicyService(t, rules)
	c, rec := newCountTokensBetaPolicyContext("interleaved-thinking-2025-05-14")
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	require.NoError(t, svc.ForwardCountTokens(context.Background(), c, newCountTokensBetaPolicyAPIKeyAccount(nil), &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-6"}))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "interleaved-thinking-2025-05-14", getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"))
}
