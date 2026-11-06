//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKiroTokenRefresherNeedsRefresh(t *testing.T) {
	r := NewKiroTokenRefresher(nil)
	window := 15 * time.Minute
	account := func(creds map[string]any) *Account {
		return &Account{ID: 1, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: creds}
	}

	require.True(t, r.NeedsRefresh(account(map[string]any{"refresh_token": "rt"}), window), "imported refresh token only")
	require.True(t, r.NeedsRefresh(account(map[string]any{"access_token": "at"}), window), "unknown expiry")
	require.True(t, r.NeedsRefresh(account(map[string]any{
		"access_token": "at",
		"expires_at":   time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}), window))
	require.False(t, r.NeedsRefresh(account(map[string]any{
		"access_token": "at",
		"expires_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
	}), window))

	other := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{}}
	require.False(t, r.CanRefresh(other))
	require.False(t, r.NeedsRefresh(other, window))
	require.Equal(t, "kiro:account:1", r.CacheKey(account(nil)))
}

func TestKiroTokenProviderReturnsAccountWithCachedToken(t *testing.T) {
	cache := newClaudeTokenCacheStub()
	snapshot := &Account{ID: 7, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "old-at", "refresh_token": "rt",
	}, Extra: map[string]any{"other": "x"}}
	cache.tokens[KiroTokenCacheKey(snapshot)] = "new-at"
	p := NewKiroTokenProvider(nil, cache, nil, nil)

	token, account, err := p.GetAccessToken(context.Background(), snapshot)
	require.NoError(t, err)
	require.Equal(t, "new-at", token)
	require.Equal(t, "new-at", account.GetCredential("access_token"),
		"the account carries the token that is sent, so a 401 marks that token")
	require.Equal(t, "rt", account.GetCredential("refresh_token"))
	require.Equal(t, "old-at", snapshot.GetCredential("access_token"), "the shared snapshot is not modified")
	account.Extra[kiroRejectedTokenExtraKey] = "fp"
	require.NotContains(t, snapshot.Extra, kiroRejectedTokenExtraKey, "the clone has its own Extra map")

	cache.tokens[KiroTokenCacheKey(snapshot)] = "old-at"
	_, same, err := p.GetAccessToken(context.Background(), snapshot)
	require.NoError(t, err)
	require.Same(t, snapshot, same, "no copy when the token is unchanged")
}
