package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	kiroTokenRefreshSkew = 5 * time.Minute
	kiroTokenCacheSkew   = 5 * time.Minute
	// kiroRequestRefreshTimeout 是请求路径上等待 token 刷新的上限，超时后
	// 由后台刷新服务继续重试。
	kiroRequestRefreshTimeout = 10 * time.Second
)

// KiroTokenRefresher 实现 Kiro 账号的 TokenRefresher 与 OAuthRefreshExecutor。
type KiroTokenRefresher struct {
	oauthService *KiroOAuthService
}

func NewKiroTokenRefresher(oauthService *KiroOAuthService) *KiroTokenRefresher {
	return &KiroTokenRefresher{oauthService: oauthService}
}

// CacheKey 返回分布式锁与 token 缓存使用的键。
func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformKiro && account.Type == AccountTypeOAuth
}

// NeedsRefresh 在 access token 缺失或即将过期时返回 true。导入时只有
// refresh token 的账号没有 access token，需要先刷新一次。
func (r *KiroTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential("access_token")) == "" {
		return true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r.oauthService == nil {
		return nil, errors.New("kiro oauth service is not configured")
	}
	return r.oauthService.RefreshAccountToken(ctx, account)
}

func KiroTokenCacheKey(account *Account) string {
	return "kiro:account:" + strconv.FormatInt(account.ID, 10)
}

// KiroTokenProvider 为请求路径提供有效的 access token。
type KiroTokenProvider struct {
	accountRepo AccountRepository
	tokenCache  GeminiTokenCache
	refreshAPI  *OAuthRefreshAPI
	executor    OAuthRefreshExecutor
}

func NewKiroTokenProvider(accountRepo AccountRepository, tokenCache GeminiTokenCache, refreshAPI *OAuthRefreshAPI, executor OAuthRefreshExecutor) *KiroTokenProvider {
	return &KiroTokenProvider{
		accountRepo: accountRepo,
		tokenCache:  tokenCache,
		refreshAPI:  refreshAPI,
		executor:    executor,
	}
}

// GetAccessToken 返回可用的 access token，即将过期时先刷新。返回的账号
// credentials 是刷新后的最新值（profile_arn 等字段可能在刷新时更新）。
func (p *KiroTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, *Account, error) {
	if account == nil || account.Platform != PlatformKiro || account.Type != AccountTypeOAuth {
		return "", account, errors.New("not a kiro oauth account")
	}
	cacheKey := KiroTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, account, nil
		}
	}

	if p.executor != nil && p.executor.NeedsRefresh(account, kiroTokenRefreshSkew) && p.refreshAPI != nil {
		refreshCtx, cancel := context.WithTimeout(ctx, kiroRequestRefreshTimeout)
		result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, kiroTokenRefreshSkew)
		cancel()
		switch {
		case err != nil:
			// 旧 token 还没过期时继续使用，否则交给调用方切换账号。
			if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt == nil || !time.Now().Before(*expiresAt) {
				return "", account, err
			}
			slog.Warn("kiro_token_refresh_failed_use_existing", "account_id", account.ID, "error", err)
		case result != nil && result.Account != nil && !result.LockHeld:
			account = result.Account
		}
	}

	token := strings.TrimSpace(account.GetCredential("access_token"))
	if token == "" {
		return "", account, errors.New("access_token not found in credentials")
	}
	if p.tokenCache != nil {
		ttl := time.Minute
		if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
			if until := time.Until(*expiresAt); until > kiroTokenCacheSkew {
				ttl = until - kiroTokenCacheSkew
			}
		}
		if err := p.tokenCache.SetAccessToken(ctx, cacheKey, token, ttl); err != nil {
			slog.Warn("kiro_token_cache_set_failed", "account_id", account.ID, "error", err)
		}
	}
	return token, account, nil
}
