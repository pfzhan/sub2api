//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func expiredSetupTokenAccount() *Account {
	return &Account{
		ID: 301, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Status: StatusActive,
		Credentials: map[string]any{
			"access_token":  "st-expired",
			"refresh_token": "rt-1",
			"expires_at":    time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
}

func setupTokenProviderWithRefresh(account *Account, executor *refreshAPIExecutorStub, cache ClaudeTokenCache) (*ClaudeTokenProvider, *refreshAPIAccountRepo) {
	repo := &refreshAPIAccountRepo{account: account}
	provider := NewClaudeTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)
	return provider, repo
}

// setup-token 过期后请求路径必须先刷新，不能把过期 token 发给上游。
func TestGatewayGetAccessToken_ExpiredSetupTokenRefreshesOnRequestPath(t *testing.T) {
	account := expiredSetupTokenAccount()
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials: map[string]any{
			"access_token":  "st-new",
			"refresh_token": "rt-2",
			"expires_at":    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		},
	}
	cache := newClaudeTokenCacheStub()
	provider, repo := setupTokenProviderWithRefresh(account, executor, cache)
	svc := &GatewayService{claudeTokenProvider: provider}

	token, tokenType, err := svc.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "st-new", token)
	require.Equal(t, "oauth", tokenType)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, "rt-2", repo.account.Credentials["refresh_token"])
	require.Empty(t, cache.tokens, "setup-token must not be written to the token cache")
}

func TestClaudeTokenProvider_SetupTokenNotExpiringSkipsRefresh(t *testing.T) {
	account := expiredSetupTokenAccount()
	account.Credentials["access_token"] = "st-valid"
	account.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	executor := &refreshAPIExecutorStub{needsRefresh: true}
	provider, _ := setupTokenProviderWithRefresh(account, executor, nil)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "st-valid", token)
	require.Zero(t, executor.refreshCalls)
}

func TestClaudeTokenProvider_SetupTokenWithoutExpiryReadsCredentials(t *testing.T) {
	account := expiredSetupTokenAccount()
	account.Credentials["access_token"] = "st-no-expiry"
	delete(account.Credentials, "expires_at")
	executor := &refreshAPIExecutorStub{needsRefresh: true}
	provider, _ := setupTokenProviderWithRefresh(account, executor, nil)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "st-no-expiry", token)
	require.Zero(t, executor.refreshCalls)
}

// 刷新失败时与 oauth 的默认策略一致：返回库内现有 token，由上游响应决定后续调度。
func TestClaudeTokenProvider_SetupTokenRefreshFailureFallsBackToExistingToken(t *testing.T) {
	account := expiredSetupTokenAccount()
	executor := &refreshAPIExecutorStub{needsRefresh: true, err: errors.New("upstream unavailable")}
	provider, _ := setupTokenProviderWithRefresh(account, executor, nil)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "st-expired", token)
	require.Equal(t, 1, executor.refreshCalls)
}
