//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func customBaseURLTestAccount(accountType string, extra map[string]any) *Account {
	proxyID := int64(5)
	return &Account{
		ID:          9101,
		Platform:    PlatformAnthropic,
		Type:        accountType,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
		Extra:       extra,
		ProxyID:     &proxyID,
		Proxy:       &Proxy{ID: proxyID, Protocol: "http", Host: "10.0.0.5", Port: 8080},
	}
}

const customBaseURLTestProxy = "http://10.0.0.5:8080"

func customBaseURLTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

// 开关与地址同时有效才走中继；任一缺失都按官方地址和账号代理发出，
// URL 构建与代理选择必须给出同一结论。
func TestAnthropicCustomBaseURL_URLAndProxyAgree(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	relayMessages := "https://relay.example.com/v1/messages?beta=true&proxy=" + "http%3A%2F%2F10.0.0.5%3A8080"
	relayCount := "https://relay.example.com/v1/messages/count_tokens?beta=true&proxy=" + "http%3A%2F%2F10.0.0.5%3A8080"

	tests := []struct {
		name      string
		extra     map[string]any
		wantURL   string
		wantCount string
		wantProxy string
	}{
		{
			name:      "enabled with url uses relay",
			extra:     map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://relay.example.com/"},
			wantURL:   relayMessages,
			wantCount: relayCount,
			wantProxy: "",
		},
		{
			name:      "enabled without url uses official endpoint",
			extra:     map[string]any{"custom_base_url_enabled": true},
			wantURL:   claudeAPIURL,
			wantCount: claudeAPICountTokensURL,
			wantProxy: customBaseURLTestProxy,
		},
		{
			name:      "enabled with blank url uses official endpoint",
			extra:     map[string]any{"custom_base_url_enabled": true, "custom_base_url": "   "},
			wantURL:   claudeAPIURL,
			wantCount: claudeAPICountTokensURL,
			wantProxy: customBaseURLTestProxy,
		},
		{
			name:      "url without switch uses official endpoint",
			extra:     map[string]any{"custom_base_url": "https://relay.example.com"},
			wantURL:   claudeAPIURL,
			wantCount: claudeAPICountTokensURL,
			wantProxy: customBaseURLTestProxy,
		},
	}
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, tt := range tests {
			t.Run(accountType+"/"+tt.name, func(t *testing.T) {
				account := customBaseURLTestAccount(accountType, tt.extra)

				req, _, err := svc.buildUpstreamRequest(context.Background(), customBaseURLTestContext(), account, body, "test-token", "oauth", "claude-sonnet-4-6", false, false)
				require.NoError(t, err)
				require.Equal(t, tt.wantURL, req.URL.String())

				countReq, _, err := svc.buildCountTokensRequest(context.Background(), customBaseURLTestContext(), account, body, "test-token", "oauth", "claude-sonnet-4-6", false)
				require.NoError(t, err)
				require.Equal(t, tt.wantCount, countReq.URL.String())

				require.Equal(t, tt.wantProxy, account.AnthropicUpstreamProxyURL())
			})
		}
	}
}

// /v1/chat/completions 与 /v1/responses 转发到中继时，连接中继不能再走账号代理，
// 与 /v1/messages 保持一致（代理已作为查询参数交给中继）。
func TestAnthropicCustomBaseURL_CompatForwardsSkipProxyForRelay(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	newUpstream := func() *httpUpstreamRecorder {
		return &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}}
	}
	extra := map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://relay.example.com"}

	t.Run("chat completions", func(t *testing.T) {
		upstream := newUpstream()
		svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

		_, err := svc.ForwardAsChatCompletions(context.Background(), c, customBaseURLTestAccount(AccountTypeOAuth, extra), body, nil)

		require.NoError(t, err)
		require.True(t, strings.HasPrefix(upstream.lastReq.URL.String(), "https://relay.example.com/v1/messages?beta=true"))
		require.Empty(t, upstream.lastProxyURL)
	})

	t.Run("responses", func(t *testing.T) {
		upstream := newUpstream()
		svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		body := []byte(`{"model":"claude-sonnet-4-6","input":"hi","stream":true}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))

		_, err := svc.ForwardAsResponses(context.Background(), c, customBaseURLTestAccount(AccountTypeOAuth, extra), body, nil)

		require.NoError(t, err)
		require.True(t, strings.HasPrefix(upstream.lastReq.URL.String(), "https://relay.example.com/v1/messages?beta=true"))
		require.Empty(t, upstream.lastProxyURL)
	})
}

// 测试连接与真实转发走同一中继判断：生效时发往中继且不走代理，不生效时直连官方并走代理。
func TestAccountTestService_ClaudeOAuthFollowsEffectiveCustomBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		extra     map[string]any
		wantURL   string
		wantProxy string
	}{
		{
			name:      "relay",
			extra:     map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://relay.example.com"},
			wantURL:   "https://relay.example.com/v1/messages?beta=true&proxy=http%3A%2F%2F10.0.0.5%3A8080",
			wantProxy: "",
		},
		{
			name:      "switch on without url",
			extra:     map[string]any{"custom_base_url_enabled": true},
			wantURL:   testClaudeAPIURL,
			wantProxy: customBaseURLTestProxy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := customBaseURLTestAccount(AccountTypeOAuth, tt.extra)
			account.Status = StatusActive
			svc, upstream := adaptiveCNAccountTestService(account, adaptiveCNAnthropicTestResponse())
			c, _ := newTestContext()

			err := svc.TestAccountConnection(c, account.ID, "claude-sonnet-4-6", "", AccountTestModeDefault)

			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, tt.wantURL, upstream.requests[0].URL.String())
			require.Equal(t, tt.wantProxy, upstream.lastProxyURL)
		})
	}
}
