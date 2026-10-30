//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func newKiroQuotaFetcherForTest(upstream *kiroFakeUpstream, now *time.Time) *KiroQuotaFetcher {
	f := NewKiroQuotaFetcher(NewKiroTokenProvider(nil, nil, nil, nil))
	f.newDoer = upstream.newDoer
	f.now = func() time.Time { return *now }
	return f
}

func TestKiroQuotaFetcherBuildsQuotaAndCaches(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"q.us-east-1.amazonaws.com/getUsageLimits": {{200, `{
			"subscriptionInfo":{"subscriptionTitle":"KIRO PRO"},
			"userInfo":{"email":"dev@example.com"},
			"usageBreakdownList":[{"displayName":"Credits","currentUsageWithPrecision":250.5,"usageLimitWithPrecision":1000,"nextDateReset":1798761600}]
		}`}},
	}}
	now := time.Now()
	f := newKiroQuotaFetcherForTest(upstream, &now)
	account := newKiroAccountForTest()

	usage, err := f.FetchUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.NotNil(t, usage.KiroQuota)
	require.Equal(t, "PRO", usage.KiroQuota.Plan)
	require.Equal(t, "dev@example.com", usage.KiroQuota.Email)
	require.InDelta(t, 250.5, usage.KiroQuota.Used, 1e-9)
	require.InDelta(t, 1000, usage.KiroQuota.Limit, 1e-9)
	require.Equal(t, time.Unix(1798761600, 0), *usage.KiroQuota.ResetsAt)
	require.Len(t, usage.KiroQuota.Items, 1)
	require.Equal(t, kiro.UsageKindBase, usage.KiroQuota.Items[0].Kind)

	_, err = f.FetchUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Len(t, upstream.calls, 1, "second call is served from cache")

	_, err = f.FetchUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Len(t, upstream.calls, 2, "force skips the cache")

	now = now.Add(apiCacheTTL)
	_, err = f.FetchUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Len(t, upstream.calls, 3, "cache expires after apiCacheTTL")
}

func TestKiroQuotaFetcherReportsUpstreamErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		body        string
		errorCode   string
		needsReauth bool
		banned      bool
	}{
		"401":           {http.StatusUnauthorized, `{"message":"expired"}`, "unauthenticated", true, false},
		"403 suspended": {http.StatusForbidden, `{"message":"Your User ID temporarily is suspended."}`, "forbidden", false, true},
		"403 token":     {http.StatusForbidden, `{"message":"The bearer token included in the request is invalid."}`, "unauthenticated", true, false},
		"429":           {http.StatusTooManyRequests, `{"message":"slow down"}`, "rate_limited", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
				"q.us-east-1.amazonaws.com/getUsageLimits": {{tc.status, tc.body}},
			}}
			now := time.Now()
			f := newKiroQuotaFetcherForTest(upstream, &now)

			usage, err := f.FetchUsage(context.Background(), newKiroAccountForTest(), false)
			require.NoError(t, err)
			require.Nil(t, usage.KiroQuota)
			require.Contains(t, usage.Error, "HTTP ")
			require.Equal(t, tc.errorCode, usage.ErrorCode)
			require.Equal(t, tc.needsReauth, usage.NeedsReauth)
			require.Equal(t, tc.banned, usage.IsBanned)

			now = now.Add(apiErrorCacheTTL - time.Second)
			_, _ = f.FetchUsage(context.Background(), newKiroAccountForTest(), false)
			require.Len(t, upstream.calls, 1, "errors are cached for apiErrorCacheTTL")
		})
	}
}

func TestAccountUsageServiceKiroNeverQueriesAnthropic(t *testing.T) {
	svc := &AccountUsageService{cache: NewUsageCache()}
	_, err := svc.GetUsageForAccount(context.Background(), newKiroAccountForTest())
	require.ErrorContains(t, err, "kiro usage query is not configured")
}
