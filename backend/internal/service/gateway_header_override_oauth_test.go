//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func headerOverrideGatewayAccount(accountType string) *Account {
	return &Account{
		ID:          9201,
		Platform:    PlatformAnthropic,
		Type:        accountType,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":            "test-token",
			"api_key":                 "sk-test",
			"header_override_enabled": true,
			"header_overrides": map[string]any{
				"x-relay-tag":    "relay",
				"anthropic-beta": "override-beta",
				"user-agent":     "override-ua",
			},
		},
	}
}

// 账号级请求头覆写只对 Anthropic API Key 账号生效；OAuth/setup-token 的 Messages 与
// count_tokens 请求必须保留网关自己设置的认证与 beta 头。
func TestAnthropicHeaderOverrides_OnlyAPIKeyAccountsOnMessagesAndCountTokens(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	build := func(t *testing.T, account *Account, token, tokenType string, mimic bool) []http.Header {
		t.Helper()
		req, _, err := svc.buildUpstreamRequest(context.Background(), customBaseURLTestContext(), account, body, token, tokenType, "claude-sonnet-4-6", false, mimic)
		require.NoError(t, err)
		countReq, _, err := svc.buildCountTokensRequest(context.Background(), customBaseURLTestContext(), account, body, token, tokenType, "claude-sonnet-4-6", mimic)
		require.NoError(t, err)
		return []http.Header{req.Header, countReq.Header}
	}

	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, mimic := range []bool{false, true} {
			account := headerOverrideGatewayAccount(accountType)
			for i, h := range build(t, account, "test-token", "oauth", mimic) {
				require.Empty(t, getHeaderRaw(h, "x-relay-tag"), "%s mimic=%v request %d", accountType, mimic, i)
				require.NotEqual(t, "override-beta", getHeaderRaw(h, "anthropic-beta"), "%s mimic=%v request %d", accountType, mimic, i)
				require.NotEqual(t, "override-ua", getHeaderRaw(h, "user-agent"), "%s mimic=%v request %d", accountType, mimic, i)
				require.Equal(t, "Bearer test-token", getHeaderRaw(h, "authorization"), "%s mimic=%v request %d", accountType, mimic, i)
			}
		}
	}

	account := headerOverrideGatewayAccount(AccountTypeAPIKey)
	for i, h := range build(t, account, "sk-test", "apikey", false) {
		require.Equal(t, "relay", getHeaderRaw(h, "x-relay-tag"), "apikey request %d", i)
		require.Equal(t, "override-beta", getHeaderRaw(h, "anthropic-beta"), "apikey request %d", i)
		require.Equal(t, "override-ua", getHeaderRaw(h, "user-agent"), "apikey request %d", i)
		require.Equal(t, "sk-test", getHeaderRaw(h, "x-api-key"), "apikey request %d", i)
	}
}

func TestIsHeaderOverrideEligible_AnthropicSetupTokenIsNotEligible(t *testing.T) {
	require.False(t, headerOverrideGatewayAccount(AccountTypeSetupToken).IsHeaderOverrideEligible())
	require.Nil(t, headerOverrideGatewayAccount(AccountTypeSetupToken).GetHeaderOverrides())
}
