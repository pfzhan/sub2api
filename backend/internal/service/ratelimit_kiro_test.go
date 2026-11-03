//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func kiroRateLimitAccount() *Account {
	return &Account{
		ID:          7,
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "at", "refresh_token": "rt"},
	}
}

func TestHandleUpstreamError_KiroQuotaExhaustedCoolsDownToNextMonth(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"402":       {http.StatusPaymentRequired, `{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`},
		"403 quota": {http.StatusForbidden, `{"message":"Monthly request limit exceeded","reason":"MONTHLY_REQUEST_COUNT"}`},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

			shouldDisable := svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), tc.status, http.Header{}, []byte(tc.body))

			require.True(t, shouldDisable)
			require.Equal(t, 1, repo.rateLimitedCalls)
			require.Equal(t, int64(7), repo.lastRateLimitedID)
			require.WithinDuration(t, kiro.QuotaResetAt(time.Now()), repo.lastRateLimitedAt, time.Second)
			require.Zero(t, repo.setErrorCalls)
		})
	}
}

func TestHandleUpstreamError_KiroForbiddenTokenInvalidActsLike401(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), http.StatusForbidden, http.Header{},
		[]byte(`{"message":"The bearer token included in the request is invalid."}`))

	require.Equal(t, 1, repo.tempCalls, "oauth account with refresh token waits for refresh")
	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.rateLimitedCalls)
}

func TestHandleUpstreamError_KiroSuspendedMarksError(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	shouldDisable := svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), http.StatusForbidden, http.Header{},
		[]byte(`{"message":"Your User ID temporarily is suspended. Contact support to appeal."}`))

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Contains(t, repo.lastErrorMsg, "Kiro account suspended (403)")
	require.Zero(t, repo.rateLimitedCalls)
}

func TestHandleKiroErrorLeavesOtherForbiddenToDefault(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	handled, _ := svc.handleKiroError(context.Background(), kiroRateLimitAccount(), http.StatusForbidden, []byte(`{"message":"Forbidden"}`), "Forbidden")

	require.False(t, handled)
	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.rateLimitedCalls)
	require.Zero(t, repo.tempCalls)
}

func TestHandleUpstreamError_KiroForbiddenThrottleIsShortCooldown(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), http.StatusForbidden, http.Header{},
		[]byte(`{"message":"Rate limit exceeded"}`))

	require.Zero(t, repo.setErrorCalls)
	require.Equal(t, 1, repo.rateLimitedCalls)
	require.WithinDuration(t, time.Now(), repo.lastRateLimitedAt, time.Minute, "throttle must not cool down to next month")
}

func TestHandleUpstreamError_KiroUnknownForbiddenCoolsDownBeforeDisabling(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{1}})

	svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), http.StatusForbidden, http.Header{},
		[]byte(`{"message":"User is not allowed to access model claude-opus-4.7"}`))

	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.rateLimitedCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, "Kiro 403 temporary cooldown")
}

func TestHandleUpstreamError_Kiro401ForcesRefreshOfRejectedToken(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := kiroRateLimitAccount()
	account.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	refresher := NewKiroTokenRefresher(nil)
	require.False(t, refresher.NeedsRefresh(account, kiroTokenRefreshSkew))

	svc.HandleUpstreamError(context.Background(), account, http.StatusUnauthorized, http.Header{},
		[]byte(`{"message":"The bearer token included in the request is invalid."}`))

	require.Equal(t, kiroTokenFingerprint("at"), repo.lastExtraUpdates[kiroRejectedTokenExtraKey])
	require.True(t, refresher.NeedsRefresh(account, kiroTokenRefreshSkew), "rejected token must be refreshed even before expiry")

	// 刷新换了新 token 后不再强制刷新。
	account.Credentials["access_token"] = "at2"
	require.False(t, refresher.NeedsRefresh(account, kiroTokenRefreshSkew))
}
