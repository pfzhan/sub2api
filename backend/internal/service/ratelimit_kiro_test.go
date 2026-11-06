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

type kiroQuotaLookupStub struct {
	usage *UsageInfo
	err   error
	calls int
	// beforeReturn 在查询返回前执行，用来模拟查询期间账号状态被改动。
	beforeReturn func()
}

func (s *kiroQuotaLookupStub) FetchUsage(context.Context, *Account, bool) (*UsageInfo, error) {
	s.calls++
	if s.beforeReturn != nil {
		s.beforeReturn()
	}
	return s.usage, s.err
}

// kiroQuotaRepoStub 模拟数据库里账号的限流字段与 extra 标记。
type kiroQuotaRepoStub struct {
	rateLimitAccountRepoStub
	current    Account
	casCalls   int
	clearCalls int
}

func newKiroQuotaRepoStub() *kiroQuotaRepoStub {
	return &kiroQuotaRepoStub{current: *kiroRateLimitAccount()}
}

func (r *kiroQuotaRepoStub) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	_ = r.rateLimitAccountRepoStub.SetRateLimited(ctx, id, resetAt)
	now := time.Now()
	r.current.RateLimitedAt, r.current.RateLimitResetAt = &now, &resetAt
	r.current.UpdatedAt = now
	return nil
}

func (r *kiroQuotaRepoStub) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	_ = r.rateLimitAccountRepoStub.UpdateExtra(ctx, id, updates)
	extra := map[string]any{}
	for k, v := range r.current.Extra {
		extra[k] = v
	}
	for k, v := range updates {
		extra[k] = v
	}
	r.current.Extra = extra
	r.current.UpdatedAt = time.Now()
	return nil
}

func (r *kiroQuotaRepoStub) GetByID(context.Context, int64) (*Account, error) {
	acc := r.current
	return &acc, nil
}

func (r *kiroQuotaRepoStub) SetRateLimitedIfUnchanged(_ context.Context, _ int64, updatedAt time.Time, limitedAt, resetAt *time.Time, newResetAt time.Time) (bool, error) {
	r.casCalls++
	if !updatedAt.Equal(r.current.UpdatedAt) || !timePtrEqual(limitedAt, r.current.RateLimitedAt) || !timePtrEqual(resetAt, r.current.RateLimitResetAt) {
		return false, nil
	}
	r.lastRateLimitedAt = newResetAt
	r.current.RateLimitResetAt = &newResetAt
	r.current.UpdatedAt = time.Now()
	return true, nil
}

func (r *kiroQuotaRepoStub) ClearRateLimitIfObserved(_ context.Context, _ int64, limitedAt, resetAt time.Time) (bool, error) {
	r.clearCalls++
	return true, nil
}

func newKiroQuotaRateLimitService(repo AccountRepository, lookup kiroQuotaLookup) *RateLimitService {
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetKiroQuotaLookup(lookup)
	svc.runAsync = func(f func()) { f() }
	return svc
}

func kiroQuota402(t *testing.T, svc *RateLimitService) {
	t.Helper()
	require.True(t, svc.HandleUpstreamError(context.Background(), kiroRateLimitAccount(), http.StatusPaymentRequired, http.Header{}, []byte(`{"message":"limit"}`)))
}

func TestHandleUpstreamError_KiroQuotaExhaustedUsesUpstreamResetTime(t *testing.T) {
	resetAt := time.Now().Add(9 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := newKiroQuotaRepoStub()
	lookup := &kiroQuotaLookupStub{usage: &UsageInfo{KiroQuota: &KiroQuotaInfo{Used: 50, Limit: 50, ResetsAt: &resetAt}}}
	kiroQuota402(t, newKiroQuotaRateLimitService(repo, lookup))

	require.Equal(t, 1, lookup.calls)
	require.Equal(t, 1, repo.casCalls)
	require.Equal(t, resetAt, *repo.current.RateLimitResetAt)
	require.Equal(t, resetAt.Format(time.RFC3339), repo.current.Extra[kiroQuotaCooldownExtraKey])
	require.True(t, kiroQuotaCooldownActive(&repo.current, time.Now()))
}

func TestHandleUpstreamError_KiroQuotaExhaustedWithCreditsLeftCoolsDownBriefly(t *testing.T) {
	repo := newKiroQuotaRepoStub()
	lookup := &kiroQuotaLookupStub{usage: &UsageInfo{KiroQuota: &KiroQuotaInfo{Used: 50, Limit: 550}}}
	kiroQuota402(t, newKiroQuotaRateLimitService(repo, lookup))

	require.WithinDuration(t, time.Now().Add(kiroQuotaMismatchCooldown), *repo.current.RateLimitResetAt, 2*time.Second)
}

func TestHandleUpstreamError_KiroQuotaFractionalCreditIsExhausted(t *testing.T) {
	resetAt := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := newKiroQuotaRepoStub()
	lookup := &kiroQuotaLookupStub{usage: &UsageInfo{KiroQuota: &KiroQuotaInfo{Used: 49.95, Limit: 50, ResetsAt: &resetAt}}}
	kiroQuota402(t, newKiroQuotaRateLimitService(repo, lookup))

	require.Equal(t, resetAt, *repo.current.RateLimitResetAt, "0.05 credits left is not enough for a request")
}

func TestHandleUpstreamError_KiroQuotaLookupFailureKeepsFallback(t *testing.T) {
	repo := newKiroQuotaRepoStub()
	lookup := &kiroQuotaLookupStub{usage: &UsageInfo{Error: "HTTP 500"}}
	kiroQuota402(t, newKiroQuotaRateLimitService(repo, lookup))

	require.Zero(t, repo.casCalls)
	require.WithinDuration(t, kiro.QuotaResetAt(time.Now()), *repo.current.RateLimitResetAt, time.Second)
}

func TestHandleUpstreamError_KiroQuotaRefineSkipsAfterManualClear(t *testing.T) {
	resetAt := time.Now().Add(9 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := newKiroQuotaRepoStub()
	lookup := &kiroQuotaLookupStub{usage: &UsageInfo{KiroQuota: &KiroQuotaInfo{Used: 50, Limit: 50, ResetsAt: &resetAt}}}
	// 查询期间管理员手动解除了限流。
	lookup.beforeReturn = func() {
		repo.current.RateLimitedAt, repo.current.RateLimitResetAt = nil, nil
		repo.current.UpdatedAt = time.Now()
	}
	kiroQuota402(t, newKiroQuotaRateLimitService(repo, lookup))

	require.Zero(t, repo.casCalls)
	require.Nil(t, repo.current.RateLimitResetAt, "a manual clear is not overridden")
}

func TestTryClearKiroQuotaCooldown(t *testing.T) {
	until := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	limitedAt := time.Now().Add(-time.Minute)
	cooling := func(marker string) *Account {
		a := kiroRateLimitAccount()
		a.RateLimitedAt = &limitedAt
		a.RateLimitResetAt = &until
		a.Extra = map[string]any{kiroQuotaCooldownExtraKey: marker}
		return a
	}
	marker := until.Format(time.RFC3339)
	fresh := time.Now()
	stale := limitedAt.Add(-time.Minute)
	usage := func(used, limit float64, at time.Time) *UsageInfo {
		return &UsageInfo{UpdatedAt: &at, KiroQuota: &KiroQuotaInfo{Used: used, Limit: limit}}
	}

	for name, tc := range map[string]struct {
		account *Account
		usage   *UsageInfo
		cleared bool
	}{
		"credits back":                {cooling(marker), usage(10, 50, fresh), true},
		"still exhausted":             {cooling(marker), usage(50, 50, fresh), false},
		"fractional credit":           {cooling(marker), usage(49.5, 50, fresh), false},
		"usage cached before cooling": {cooling(marker), usage(10, 50, stale), false},
		"limited for another cause":   {cooling(until.Add(-time.Hour).Format(time.RFC3339)), usage(10, 50, fresh), false},
		"no marker":                   {cooling(""), usage(10, 50, fresh), false},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newKiroQuotaRepoStub()
			svc := &AccountUsageService{accountRepo: repo}
			svc.tryClearKiroQuotaCooldown(context.Background(), tc.account, tc.usage)
			if tc.cleared {
				require.Equal(t, 1, repo.clearCalls)
				require.Nil(t, tc.account.RateLimitResetAt)
			} else {
				require.Zero(t, repo.clearCalls)
			}
		})
	}
}
