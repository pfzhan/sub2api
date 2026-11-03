package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

const kiroQuotaExhaustedReason = "kiro_quota_exhausted"

// handleKiroError 处理 Kiro 的 402 与 403。返回 handled=false 时由默认逻辑处理。
//
//   - 402 与额度类 403：月度额度用尽，冷却到下月 1 日 00:00 UTC；
//   - token 失效类 403：按 401 处理，临时停调并等待刷新；
//   - 停用、违反条款类 403：需要人工处理，标记账号异常；
//   - 限流类 403：按 429 冷却；
//   - 其余 403 交给 handle403：先临时冷却，窗口内累计多次再标记异常，避免
//     一次模型或功能级的 403 直接停用账号。
func (s *RateLimitService) handleKiroError(ctx context.Context, account *Account, statusCode int, responseBody []byte, upstreamMsg string) (handled bool, shouldDisable bool) {
	kind := kiro.ForbiddenOther
	if statusCode == http.StatusForbidden {
		kind = kiro.ClassifyForbidden(responseBody)
	}
	switch {
	case statusCode == http.StatusPaymentRequired || kind == kiro.ForbiddenQuota:
		until := kiro.QuotaResetAt(time.Now())
		s.notifyAccountSchedulingBlocked(account, until, kiroQuotaExhaustedReason)
		if err := s.accountRepo.SetRateLimited(ctx, account.ID, until); err != nil {
			slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
		}
		slog.Info("kiro_account_quota_exhausted",
			"account_id", account.ID,
			"status_code", statusCode,
			"reset_at", until,
			"message", upstreamMsg,
		)
		return true, true
	case kind == kiro.ForbiddenTokenInvalid:
		return true, s.HandleUpstreamError(ctx, account, http.StatusUnauthorized, nil, responseBody)
	case kind == kiro.ForbiddenSuspended:
		msg := "Kiro account suspended (403)"
		if upstreamMsg != "" {
			msg += ": " + upstreamMsg
		}
		s.handleAuthError(ctx, account, msg)
		return true, true
	case kind == kiro.ForbiddenThrottled:
		return true, s.HandleUpstreamError(ctx, account, http.StatusTooManyRequests, nil, responseBody)
	}
	return false, false
}

// markKiroTokenRejected 记录被上游拒绝的 access token，下次取 token 时先刷新。
// 只清缓存不够：token 还没到过期时间时 NeedsRefresh 返回 false，冷却结束后会
// 继续用同一个被拒绝的 token。
func (s *RateLimitService) markKiroTokenRejected(ctx context.Context, account *Account) {
	token := strings.TrimSpace(account.GetCredential("access_token"))
	if token == "" {
		return
	}
	updates := map[string]any{kiroRejectedTokenExtraKey: kiroTokenFingerprint(token)}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("kiro_401_force_refresh_mark_failed", "account_id", account.ID, "error", err)
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any, len(updates))
	}
	for k, v := range updates {
		account.Extra[k] = v
	}
}
