//go:build unit

package service

import (
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
