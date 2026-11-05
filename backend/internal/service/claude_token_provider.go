package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	claudeTokenRefreshSkew = 3 * time.Minute
	claudeTokenCacheSkew   = 5 * time.Minute
	claudeLockWaitTime     = 200 * time.Millisecond

	// claudeRejectedTokenExtraKey 记录被上游 401 拒绝的 access token 指纹。当前 token
	// 与指纹一致时必须先刷新；刷新换了新 token 后指纹自然失效，不需要清除。
	claudeRejectedTokenExtraKey = "claude_rejected_token_hash"
)

// claudeTokenFingerprint 返回 access token 的短指纹，避免把 token 明文写进 extra。
func claudeTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:8])
}

// claudeTokenRejected 报告账号当前的 access token 是否已被上游拒绝。
func claudeTokenRejected(account *Account) bool {
	token := strings.TrimSpace(account.GetCredential("access_token"))
	marker := account.getExtraString(claudeRejectedTokenExtraKey)
	return token != "" && marker != "" && marker == claudeTokenFingerprint(token)
}

// ClaudeTokenCache token cache interface.
type ClaudeTokenCache = GeminiTokenCache

// ClaudeTokenProvider manages access_token for Claude OAuth and Vertex service account accounts.
type ClaudeTokenProvider struct {
	accountRepo   AccountRepository
	tokenCache    ClaudeTokenCache
	oauthService  *OAuthService
	refreshAPI    *OAuthRefreshAPI
	executor      OAuthRefreshExecutor
	refreshPolicy ProviderRefreshPolicy
}

func NewClaudeTokenProvider(
	accountRepo AccountRepository,
	tokenCache ClaudeTokenCache,
	oauthService *OAuthService,
) *ClaudeTokenProvider {
	return &ClaudeTokenProvider{
		accountRepo:   accountRepo,
		tokenCache:    tokenCache,
		oauthService:  oauthService,
		refreshPolicy: ClaudeProviderRefreshPolicy(),
	}
}

// SetRefreshAPI injects unified OAuth refresh API and executor.
func (p *ClaudeTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

// SetRefreshPolicy injects caller-side refresh policy.
func (p *ClaudeTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

// GetAccessToken returns a valid access_token.
func (p *ClaudeTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformAnthropic || (account.Type != AccountTypeOAuth && account.Type != AccountTypeSetupToken && account.Type != AccountTypeServiceAccount) {
		return "", errors.New("not an anthropic oauth or service account")
	}
	if account.Type == AccountTypeServiceAccount {
		return p.getServiceAccountAccessToken(ctx, account)
	}
	if account.Type == AccountTypeSetupToken {
		return p.getSetupTokenAccessToken(ctx, account)
	}

	cacheKey := ClaudeTokenCacheKey(account)

	// 1) Try cache first. A cached token that upstream already rejected is a miss.
	rejectedMarker := account.getExtraString(claudeRejectedTokenExtraKey)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" &&
			(rejectedMarker == "" || claudeTokenFingerprint(token) != rejectedMarker) {
			slog.Debug("claude_token_cache_hit", "account_id", account.ID)
			return token, nil
		} else if err != nil {
			slog.Warn("claude_token_cache_get_failed", "account_id", account.ID, "error", err)
		}
	}

	slog.Debug("claude_token_cache_miss", "account_id", account.ID)

	// 2) Refresh if needed (pre-expiry skew, or the current token was rejected by upstream).
	expiresAt := account.GetCredentialAsTime("expires_at")
	needsRefresh := expiresAt == nil || time.Until(*expiresAt) <= claudeTokenRefreshSkew || claudeTokenRejected(account)
	refreshFailed := false

	if needsRefresh && p.refreshAPI != nil && p.executor != nil {
		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, claudeTokenRefreshSkew)
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
			slog.Warn("claude_token_refresh_failed", "account_id", account.ID, "error", err)
			refreshFailed = true
		} else if result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache && p.tokenCache != nil {
				time.Sleep(claudeLockWaitTime)
				if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
					slog.Debug("claude_token_cache_hit_after_wait", "account_id", account.ID)
					return token, nil
				}
			}
		} else {
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	} else if needsRefresh && p.tokenCache != nil {
		// Backward-compatible test path when refreshAPI is not injected.
		locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second)
		if lockErr == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		} else if lockErr != nil {
			slog.Warn("claude_token_lock_failed", "account_id", account.ID, "error", lockErr)
		} else {
			time.Sleep(claudeLockWaitTime)
			if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
				slog.Debug("claude_token_cache_hit_after_wait", "account_id", account.ID)
				return token, nil
			}
		}
	}

	accessToken := account.GetCredential("access_token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("access_token not found in credentials")
	}

	// 3) Populate cache with TTL.
	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			slog.Debug("claude_token_version_stale_use_latest", "account_id", account.ID)
			accessToken = latestAccount.GetCredential("access_token")
			if strings.TrimSpace(accessToken) == "" {
				return "", errors.New("access_token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if refreshFailed {
				if p.refreshPolicy.FailureTTL > 0 {
					ttl = p.refreshPolicy.FailureTTL
				} else {
					ttl = time.Minute
				}
				slog.Debug("claude_token_cache_short_ttl", "account_id", account.ID, "reason", "refresh_failed")
			} else if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > claudeTokenCacheSkew:
					ttl = until - claudeTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			if err := p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl); err != nil {
				slog.Warn("claude_token_cache_set_failed", "account_id", account.ID, "error", err)
			}
		}
	}

	return accessToken, nil
}

// getSetupTokenAccessToken 在 setup-token 临近过期时走与 oauth 相同的刷新入口。
// setup-token 不写 token 缓存：缓存失效只覆盖 oauth 类型，后台改凭据后这里仍需读到库内的值。
// 没有 expires_at 的 setup-token 视为不过期，直接返回库内 token。
func (p *ClaudeTokenProvider) getSetupTokenAccessToken(ctx context.Context, account *Account) (string, error) {
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt != nil && time.Until(*expiresAt) <= claudeTokenRefreshSkew && p.refreshAPI != nil && p.executor != nil {
		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, claudeTokenRefreshSkew)
		switch {
		case err != nil:
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
			slog.Warn("claude_setup_token_refresh_failed", "account_id", account.ID, "error", err)
		case result.LockHeld:
			// 其他 worker 正在刷新，稍等后从库里取它写入的新 token。
			if p.accountRepo != nil {
				time.Sleep(claudeLockWaitTime)
				if latest, getErr := p.accountRepo.GetByID(ctx, account.ID); getErr == nil && latest != nil {
					account = latest
				}
			}
		case result.Account != nil:
			account = result.Account
		}
	}

	accessToken := account.GetCredential("access_token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("access_token not found in credentials")
	}
	return accessToken, nil
}

func (p *ClaudeTokenProvider) getServiceAccountAccessToken(ctx context.Context, account *Account) (string, error) {
	return getVertexServiceAccountAccessToken(ctx, p.tokenCache, account)
}
