package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// kiroLockWaitInterval 与 kiroLockWaitTimeout 控制刷新锁被其他 worker
	// 持有时，等待对方刷新完成的轮询间隔与上限。
	kiroLockWaitInterval = 200 * time.Millisecond
	kiroLockWaitTimeout  = 3 * time.Second

	// kiroRejectedTokenExtraKey 记录被上游拒绝（401 或 token 失效类 403）的
	// access token 指纹。当前 token 与指纹一致时必须先刷新；刷新换了新 token
	// 后指纹自然失效，不需要清除。
	kiroRejectedTokenExtraKey = "kiro_rejected_token_hash"
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

// NeedsRefresh 在 access token 缺失、已被上游拒绝或即将过期时返回 true。
// 导入时只有 refresh token 的账号没有 access token，需要先刷新一次。
func (r *KiroTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential("access_token")) == "" {
		return true
	}
	if kiroTokenRejected(account) {
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

// kiroTokenFingerprint 返回 access token 的短指纹，避免把 token 明文写进 extra。
func kiroTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:8])
}

// kiroTokenRejected 报告账号当前的 access token 是否已被上游拒绝。
func kiroTokenRejected(account *Account) bool {
	token := strings.TrimSpace(account.GetCredential("access_token"))
	marker := account.getExtraString(kiroRejectedTokenExtraKey)
	return token != "" && marker != "" && marker == kiroTokenFingerprint(token)
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
		case result != nil && result.LockHeld:
			if token, fresh := p.waitForConcurrentRefresh(ctx, account); token != "" {
				return token, account, nil
			} else if fresh != nil {
				account = fresh
			}
		case result != nil && result.Account != nil:
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

// waitForConcurrentRefresh 在刷新锁被其他 worker 持有时等待对方刷新完成。
// 缓存里出现 token 时返回该 token；DB 中的账号已不需要刷新时返回该账号。
// 等待超时返回空值，调用方继续使用现有 token。
func (p *KiroTokenProvider) waitForConcurrentRefresh(ctx context.Context, account *Account) (string, *Account) {
	cacheKey := KiroTokenCacheKey(account)
	deadline := time.Now().Add(kiroLockWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(kiroLockWaitInterval):
		}
		if p.tokenCache != nil {
			if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
				return token, nil
			}
		}
		if p.accountRepo == nil {
			continue
		}
		fresh, err := p.accountRepo.GetByID(ctx, account.ID)
		if err == nil && fresh != nil && !p.executor.NeedsRefresh(fresh, kiroTokenRefreshSkew) {
			return "", fresh
		}
	}
	return "", nil
}
