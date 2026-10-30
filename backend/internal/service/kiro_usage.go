package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"golang.org/x/sync/singleflight"
)

const kiroQuotaFetchTimeout = 30 * time.Second

// KiroQuotaInfo 是 Kiro 账号的额度，来自 getUsageLimits。Used / Limit 汇总
// 未过期的月度额度、赠送额度和试用额度。
type KiroQuotaInfo struct {
	Plan     string          `json:"plan,omitempty"`
	Email    string          `json:"email,omitempty"`
	Used     float64         `json:"used"`
	Limit    float64         `json:"limit"`
	ResetsAt *time.Time      `json:"resets_at,omitempty"`
	Items    []KiroQuotaItem `json:"items,omitempty"`
}

// KiroQuotaItem 是一项额度。Kind 取值见 kiro.UsageKind*。
type KiroQuotaItem struct {
	Kind     string     `json:"kind"`
	Label    string     `json:"label"`
	Used     float64    `json:"used"`
	Limit    float64    `json:"limit"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	Expired  bool       `json:"expired,omitempty"`
}

type kiroQuotaCacheEntry struct {
	usage     *UsageInfo
	timestamp time.Time
}

// KiroQuotaFetcher 查询 Kiro 账号额度，结果按账号缓存。
type KiroQuotaFetcher struct {
	tokenProvider *KiroTokenProvider
	cache         sync.Map // accountID -> *kiroQuotaCacheEntry
	flight        singleflight.Group

	// newDoer 创建上游 HTTP 客户端，测试中可替换。
	newDoer func(proxyURL string) (kiro.Doer, error)
	now     func() time.Time
}

func NewKiroQuotaFetcher(tokenProvider *KiroTokenProvider) *KiroQuotaFetcher {
	return &KiroQuotaFetcher{
		tokenProvider: tokenProvider,
		newDoer:       newKiroHTTPDoer,
		now:           time.Now,
	}
}

// SetKiroQuotaFetcher 注入 Kiro 额度查询。未注入时 Kiro 账号不支持用量查询。
func (s *AccountUsageService) SetKiroQuotaFetcher(f *KiroQuotaFetcher) {
	s.kiroQuotaFetcher = f
}

func (s *AccountUsageService) getKiroUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if s.kiroQuotaFetcher == nil {
		return nil, errors.New("kiro usage query is not configured")
	}
	usage, err := s.kiroQuotaFetcher.FetchUsage(ctx, account, force)
	if err == nil && usage != nil && usage.Error == "" {
		s.tryClearRecoverableAccountError(ctx, account)
	}
	return usage, err
}

// FetchUsage 返回账号额度。查询失败时返回带 Error 的 UsageInfo，便于前端
// 展示原因。成功结果缓存 apiCacheTTL，失败结果缓存 apiErrorCacheTTL，force
// 跳过缓存。
func (f *KiroQuotaFetcher) FetchUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if account == nil || !account.IsKiro() || account.Type != AccountTypeOAuth {
		return nil, errors.New("not a kiro oauth account")
	}
	if !force {
		if usage, ok := f.cached(account.ID); ok {
			return usage, nil
		}
	}
	result, err, _ := f.flight.Do(fmt.Sprintf("kiro-usage:%d", account.ID), func() (any, error) {
		if !force {
			if usage, ok := f.cached(account.ID); ok {
				return usage, nil
			}
		}
		// 使用独立 context，调用方取消不影响共享同一次查询的其他请求。
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kiroQuotaFetchTimeout)
		defer cancel()
		usage := f.fetch(fetchCtx, account)
		f.cache.Store(account.ID, &kiroQuotaCacheEntry{usage: usage, timestamp: f.now()})
		return usage, nil
	})
	if err != nil {
		return nil, err
	}
	usage, _ := result.(*UsageInfo)
	return usage, nil
}

func (f *KiroQuotaFetcher) cached(accountID int64) (*UsageInfo, bool) {
	value, ok := f.cache.Load(accountID)
	if !ok {
		return nil, false
	}
	entry, ok := value.(*kiroQuotaCacheEntry)
	if !ok || entry.usage == nil {
		return nil, false
	}
	ttl := apiCacheTTL
	if entry.usage.Error != "" {
		ttl = apiErrorCacheTTL
	}
	if f.now().Sub(entry.timestamp) >= ttl {
		return nil, false
	}
	return entry.usage, true
}

func (f *KiroQuotaFetcher) fetch(ctx context.Context, account *Account) *UsageInfo {
	now := f.now()
	usage := &UsageInfo{Source: "active", UpdatedAt: &now}
	if f.tokenProvider == nil {
		usage.Error = "kiro token provider is not configured"
		return usage
	}
	token, account, err := f.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		usage.Error = sanitizeUpstreamErrorMessage(err.Error())
		usage.ErrorCode = "unauthenticated"
		usage.NeedsReauth = isNonRetryableRefreshError(err)
		return usage
	}
	creds := kiro.CredentialsFromMap(account.Credentials)
	creds.AccessToken = token

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	doer, err := f.newDoer(proxyURL)
	if err != nil {
		usage.Error = "create kiro http client: " + err.Error()
		usage.ErrorCode = "network_error"
		return usage
	}
	limits, err := kiro.GetUsageLimits(ctx, doer, creds)
	if err != nil {
		applyKiroUsageError(usage, err)
		return usage
	}
	usage.KiroQuota = buildKiroQuotaInfo(limits)
	return usage
}

// applyKiroUsageError 把查询失败的原因写进 UsageInfo。只用于展示，不改变账号状态。
func applyKiroUsageError(usage *UsageInfo, err error) {
	var httpErr *kiro.HTTPError
	if !errors.As(err, &httpErr) {
		usage.Error = sanitizeUpstreamErrorMessage(err.Error())
		usage.ErrorCode = "network_error"
		return
	}
	message := strings.TrimSpace(kiro.ParseUpstreamError(httpErr.Body).Message)
	if message == "" {
		message = http.StatusText(httpErr.StatusCode)
	}
	usage.Error = sanitizeUpstreamErrorMessage(fmt.Sprintf("HTTP %d: %s", httpErr.StatusCode, message))
	switch httpErr.StatusCode {
	case http.StatusUnauthorized:
		usage.ErrorCode = "unauthenticated"
		usage.NeedsReauth = true
	case http.StatusForbidden:
		usage.ErrorCode = "forbidden"
		switch kiro.ClassifyForbidden(httpErr.Body) {
		case kiro.ForbiddenSuspended:
			usage.IsForbidden = true
			usage.IsBanned = true
			usage.ForbiddenType = "violation"
			usage.ForbiddenReason = message
		case kiro.ForbiddenTokenInvalid:
			usage.ErrorCode = "unauthenticated"
			usage.NeedsReauth = true
		default:
			usage.IsForbidden = true
			usage.ForbiddenType = "forbidden"
			usage.ForbiddenReason = message
		}
	case http.StatusTooManyRequests:
		usage.ErrorCode = "rate_limited"
	default:
		usage.ErrorCode = "network_error"
	}
}

func buildKiroQuotaInfo(limits *kiro.UsageLimits) *KiroQuotaInfo {
	used, limit := limits.Totals()
	info := &KiroQuotaInfo{
		Plan:     strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(limits.SubscriptionTitle), "KIRO ")),
		Email:    limits.Email,
		Used:     used,
		Limit:    limit,
		ResetsAt: optionalTime(limits.NextReset),
		Items:    make([]KiroQuotaItem, 0, len(limits.Items)),
	}
	for _, item := range limits.Items {
		info.Items = append(info.Items, KiroQuotaItem{
			Kind:     item.Kind,
			Label:    item.Label,
			Used:     item.Used,
			Limit:    item.Limit,
			ResetsAt: optionalTime(item.ResetsAt),
			Expired:  item.Expired,
		})
	}
	return info
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
