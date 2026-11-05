//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func anthropicOAuthAccountWithFreshToken() *Account {
	return &Account{
		ID: 401, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"access_token":  "at-rejected",
			"refresh_token": "rt-1",
			"expires_at":    time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		},
	}
}

// claudeRefreshExecutorStub 用真实的 ClaudeTokenRefresher 判断是否需要刷新，只替换上游刷新调用。
type claudeRefreshExecutorStub struct {
	*ClaudeTokenRefresher
	credentials  map[string]any
	refreshCalls int
}

func (e *claudeRefreshExecutorStub) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	e.refreshCalls++
	return MergeCredentials(account.Credentials, e.credentials), nil
}

// 未到过期时间但被上游 401 拒绝的 token，冷却结束后必须先刷新，不能继续复用。
func TestHandleUpstreamError_AnthropicOAuth401ForcesRefreshOfRejectedToken(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := anthropicOAuthAccountWithFreshToken()
	refresher := &ClaudeTokenRefresher{}
	require.False(t, refresher.NeedsRefresh(account, claudeTokenRefreshSkew))

	svc.HandleUpstreamError(context.Background(), account, http.StatusUnauthorized, http.Header{},
		[]byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`))

	require.Equal(t, 1, repo.tempCalls, "OAuth 401 keeps the temporary cooldown")
	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, claudeTokenFingerprint("at-rejected"), repo.lastExtraUpdates[claudeRejectedTokenExtraKey])
	require.NotContains(t, repo.lastExtraUpdates[claudeRejectedTokenExtraKey], "at-rejected")
	require.True(t, refresher.NeedsRefresh(account, claudeTokenRefreshSkew), "rejected token must be refreshed even before expiry")

	// 刷新换了新 token 后不再强制刷新。
	account.Credentials["access_token"] = "at-new"
	require.False(t, refresher.NeedsRefresh(account, claudeTokenRefreshSkew))
}

func TestClaudeTokenProvider_RefreshesRejectedTokenBeforeExpiry(t *testing.T) {
	account := anthropicOAuthAccountWithFreshToken()
	account.Extra = map[string]any{claudeRejectedTokenExtraKey: claudeTokenFingerprint("at-rejected")}
	executor := &claudeRefreshExecutorStub{
		ClaudeTokenRefresher: &ClaudeTokenRefresher{},
		credentials: map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-2",
			"expires_at":    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		},
	}
	repo := &refreshAPIAccountRepo{account: account}
	cache := newClaudeTokenCacheStub()
	// 被拒绝前写入缓存的旧 token 也不能再用。
	cache.tokens[ClaudeTokenCacheKey(account)] = "at-rejected"
	provider := NewClaudeTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "at-new", token)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, "rt-2", repo.account.Credentials["refresh_token"])
}

func TestClaudeTokenProvider_UnrejectedFreshTokenSkipsRefresh(t *testing.T) {
	account := anthropicOAuthAccountWithFreshToken()
	account.Credentials["access_token"] = "at-current"
	account.Extra = map[string]any{claudeRejectedTokenExtraKey: claudeTokenFingerprint("at-old")}
	executor := &claudeRefreshExecutorStub{ClaudeTokenRefresher: &ClaudeTokenRefresher{}}
	repo := &refreshAPIAccountRepo{account: account}
	cache := newClaudeTokenCacheStub()
	cache.tokens[ClaudeTokenCacheKey(account)] = "at-current"
	provider := NewClaudeTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)

	token, err := provider.GetAccessToken(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, "at-current", token)
	require.Zero(t, executor.refreshCalls)
}
