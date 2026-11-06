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
		marked := s.setKiroQuotaCooldown(ctx, account, until)
		slog.Info("kiro_account_quota_exhausted",
			"account_id", account.ID,
			"status_code", statusCode,
			"reset_at", until,
			"message", upstreamMsg,
		)
		if marked {
			s.refineKiroQuotaCooldown(ctx, account, until)
		}
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

// kiroQuotaLookup 查询 Kiro 账号额度，由 KiroQuotaFetcher 实现。
type kiroQuotaLookup interface {
	FetchUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error)
}

// SetKiroQuotaLookup 注入 Kiro 额度查询（可选）。未注入时额度耗尽一律冷却到下月 1 日。
func (s *RateLimitService) SetKiroQuotaLookup(lookup kiroQuotaLookup) {
	s.kiroQuota = lookup
}

const (
	// kiroQuotaCooldownExtraKey 记录额度耗尽冷却的截止时间（RFC3339）。账号的
	// rate_limit_reset_at 仍等于这个值时，说明当前限流就是额度冷却，额度恢复后
	// 可以提前解除。
	kiroQuotaCooldownExtraKey = "kiro_quota_cooldown_until"
	// kiroQuotaMismatchCooldown 是上游报额度耗尽、额度接口却显示还有余量时的
	// 冷却时间，避免两边数据不同步时把账号停到月底。
	kiroQuotaMismatchCooldown = 10 * time.Minute
)

// setKiroQuotaCooldown 把账号限流到 until，并记下这是额度冷却。返回是否写入成功。
func (s *RateLimitService) setKiroQuotaCooldown(ctx context.Context, account *Account, until time.Time) bool {
	until = until.UTC().Truncate(time.Second)
	s.notifyAccountSchedulingBlocked(account, until, kiroQuotaExhaustedReason)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, until); err != nil {
		slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
		return false
	}
	s.markKiroQuotaCooldown(ctx, account.ID, until)
	return true
}

func (s *RateLimitService) markKiroQuotaCooldown(ctx context.Context, accountID int64, until time.Time) {
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		kiroQuotaCooldownExtraKey: until.UTC().Truncate(time.Second).Format(time.RFC3339),
	}); err != nil {
		slog.Warn("kiro_quota_cooldown_mark_failed", "account_id", accountID, "error", err)
	}
}

// kiroRateLimitCASRepository 只在账号仍处于调用方看到的那次限流时写入新的
// 重置时间，由具体的仓库实现。
type kiroRateLimitCASRepository interface {
	SetRateLimitedIfUnchanged(ctx context.Context, id int64, expectedUpdatedAt time.Time, expectedLimitedAt, expectedResetAt *time.Time, newResetAt time.Time) (bool, error)
}

// refineKiroQuotaCooldown 在后台查询账号额度，把冷却时间改成上游的真实重置
// 时间：月度额度不一定在每月 1 日重置；如果还有可用的赠送或试用额度，只短暂
// 冷却。查询失败时保留 fallback。查询期间限流被手动解除、或被别的限流覆盖时
// 不再改写。
func (s *RateLimitService) refineKiroQuotaCooldown(ctx context.Context, account *Account, fallback time.Time) {
	if s.kiroQuota == nil || s.runAsync == nil {
		return
	}
	casRepo, ok := s.accountRepo.(kiroRateLimitCASRepository)
	if !ok {
		return
	}
	fallback = fallback.UTC().Truncate(time.Second)
	bgCtx := context.WithoutCancel(ctx)
	s.runAsync(func() {
		usage, err := s.kiroQuota.FetchUsage(bgCtx, account, true)
		if err != nil || usage == nil || usage.Error != "" || usage.KiroQuota == nil {
			return
		}
		until, ok := kiroQuotaCooldownUntil(usage.KiroQuota, time.Now())
		if !ok || until.Equal(fallback) {
			return
		}
		current, err := s.accountRepo.GetByID(bgCtx, account.ID)
		if err != nil || current == nil || !kiroQuotaCooldownActive(current, time.Now()) ||
			!current.RateLimitResetAt.UTC().Truncate(time.Second).Equal(fallback) {
			return
		}
		updated, err := casRepo.SetRateLimitedIfUnchanged(bgCtx, account.ID, current.UpdatedAt, current.RateLimitedAt, current.RateLimitResetAt, until)
		if err != nil {
			slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
			return
		}
		if !updated {
			return
		}
		s.markKiroQuotaCooldown(bgCtx, account.ID, until)
		s.notifyAccountSchedulingBlocked(current, until, kiroQuotaExhaustedReason)
		slog.Info("kiro_account_quota_cooldown_refined",
			"account_id", account.ID,
			"reset_at", until,
			"used", usage.KiroQuota.Used,
			"limit", usage.KiroQuota.Limit,
		)
	})
}

// kiroMinUsableCredits 是认为账号还能接请求的最少剩余额度。剩余零点几个
// credit 时单次请求仍可能超额，不算有余量。
const kiroMinUsableCredits = 1.0

// kiroQuotaHasCredits 报告额度查询结果是否还有可用余量。
func kiroQuotaHasCredits(quota *KiroQuotaInfo) bool {
	return quota != nil && quota.Limit-quota.Used >= kiroMinUsableCredits
}

// kiroQuotaCooldownUntil 根据额度查询结果计算冷却截止时间。
func kiroQuotaCooldownUntil(quota *KiroQuotaInfo, now time.Time) (time.Time, bool) {
	if kiroQuotaHasCredits(quota) {
		return now.Add(kiroQuotaMismatchCooldown).UTC().Truncate(time.Second), true
	}
	if quota.ResetsAt != nil && quota.ResetsAt.After(now) {
		return quota.ResetsAt.UTC().Truncate(time.Second), true
	}
	return time.Time{}, false
}

// kiroQuotaCooldownActive 报告账号当前的限流是否就是额度冷却。
func kiroQuotaCooldownActive(account *Account, now time.Time) bool {
	if account == nil || account.RateLimitResetAt == nil || !now.Before(*account.RateLimitResetAt) {
		return false
	}
	marked, err := time.Parse(time.RFC3339, account.getExtraString(kiroQuotaCooldownExtraKey))
	if err != nil {
		return false
	}
	return marked.Equal(account.RateLimitResetAt.UTC().Truncate(time.Second))
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
