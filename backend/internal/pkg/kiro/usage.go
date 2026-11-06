package kiro

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// usageResourceType 是 Kiro 对话请求的计量类型，额度查询只看这一项。
const usageResourceType = "AGENTIC_REQUEST"

// UsageLimits 是 getUsageLimits 返回的账号额度。
type UsageLimits struct {
	// SubscriptionTitle 是订阅名称，例如 "KIRO FREE"、"KIRO PRO"。
	SubscriptionTitle string
	Email             string
	// NextReset 是月度额度的下次重置时间，上游未返回时为零值。
	NextReset time.Time
	Items     []UsageItem
}

// 额度条目的类型。
const (
	UsageKindBase      = "base"
	UsageKindBonus     = "bonus"
	UsageKindFreeTrial = "free_trial"
)

// UsageItem 是一项额度：订阅自带的月度额度、赠送额度或试用额度。
type UsageItem struct {
	Kind  string
	Label string
	Used  float64
	Limit float64
	// ResetsAt 对月度额度是下次重置时间，对赠送和试用额度是过期时间。
	ResetsAt time.Time
	Expired  bool
}

// Totals 汇总未过期条目的已用量和总额度。
func (u *UsageLimits) Totals() (used, limit float64) {
	if u == nil {
		return 0, 0
	}
	for _, item := range u.Items {
		if item.Expired {
			continue
		}
		used += item.Used
		limit += item.Limit
	}
	return used, limit
}

type usageAmount struct {
	CurrentUsage              *float64 `json:"currentUsage"`
	CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
	UsageLimit                *float64 `json:"usageLimit"`
	UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
}

func (a usageAmount) values() (used, limit float64) {
	return firstFloat(a.CurrentUsageWithPrecision, a.CurrentUsage), firstFloat(a.UsageLimitWithPrecision, a.UsageLimit)
}

type usageLimitsResponse struct {
	SubscriptionInfo struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
	} `json:"subscriptionInfo"`
	UserInfo struct {
		Email string `json:"email"`
	} `json:"userInfo"`
	NextDateReset      any `json:"nextDateReset"`
	UsageBreakdownList []struct {
		usageAmount
		ResourceType  string `json:"resourceType"`
		DisplayName   string `json:"displayName"`
		NextDateReset any    `json:"nextDateReset"`
		Bonuses       []struct {
			usageAmount
			BonusCode   string `json:"bonusCode"`
			DisplayName string `json:"displayName"`
			ExpiresAt   any    `json:"expiresAt"`
			Status      string `json:"status"`
		} `json:"bonuses"`
		FreeTrialInfo *struct {
			usageAmount
			FreeTrialExpiry any    `json:"freeTrialExpiry"`
			FreeTrialStatus string `json:"freeTrialStatus"`
		} `json:"freeTrialInfo"`
	} `json:"usageBreakdownList"`
}

// GetUsageLimits 查询账号当前的额度使用情况。
func GetUsageLimits(ctx context.Context, doer Doer, creds Credentials) (*UsageLimits, error) {
	query := url.Values{}
	query.Set("isEmailRequired", "true")
	query.Set("origin", originAIEditor)
	query.Set("resourceType", usageResourceType)
	if arn := strings.TrimSpace(creds.ProfileARN); arn != "" {
		query.Set("profileArn", arn)
	}
	endpoint := strings.TrimSuffix(GenerateURL(creds), "/generateAssistantResponse") + "/getUsageLimits?" + query.Encode()
	headers := ideHeaders(creds)
	headers.Set("Authorization", "Bearer "+creds.AccessToken)

	var resp usageLimitsResponse
	if err := doJSON(ctx, doer, "usage limits", http.MethodGet, endpoint, nil, headers, &resp); err != nil {
		return nil, err
	}
	return resp.toUsageLimits(time.Now()), nil
}

func (r *usageLimitsResponse) toUsageLimits(now time.Time) *UsageLimits {
	out := &UsageLimits{
		SubscriptionTitle: strings.TrimSpace(r.SubscriptionInfo.SubscriptionTitle),
		Email:             strings.TrimSpace(r.UserInfo.Email),
		NextReset:         timeValue(r.NextDateReset),
	}
	for _, b := range r.UsageBreakdownList {
		used, limit := b.values()
		resetsAt := timeValue(b.NextDateReset)
		if out.NextReset.IsZero() {
			out.NextReset = resetsAt
		}
		out.Items = append(out.Items, UsageItem{
			Kind:     UsageKindBase,
			Label:    firstNonEmpty(b.DisplayName, b.ResourceType),
			Used:     used,
			Limit:    limit,
			ResetsAt: resetsAt,
		})
		for _, bonus := range b.Bonuses {
			used, limit := bonus.values()
			expiresAt := timeValue(bonus.ExpiresAt)
			out.Items = append(out.Items, UsageItem{
				Kind:     UsageKindBonus,
				Label:    firstNonEmpty(bonus.DisplayName, bonus.BonusCode),
				Used:     used,
				Limit:    limit,
				ResetsAt: expiresAt,
				Expired:  isExpired(bonus.Status, expiresAt, now),
			})
		}
		if ft := b.FreeTrialInfo; ft != nil {
			used, limit := ft.values()
			expiresAt := timeValue(ft.FreeTrialExpiry)
			out.Items = append(out.Items, UsageItem{
				Kind:     UsageKindFreeTrial,
				Label:    "Free Trial",
				Used:     used,
				Limit:    limit,
				ResetsAt: expiresAt,
				Expired:  isExpired(ft.FreeTrialStatus, expiresAt, now),
			})
		}
	}
	return out
}

func isExpired(status string, expiresAt, now time.Time) bool {
	if strings.EqualFold(strings.TrimSpace(status), "EXPIRED") {
		return true
	}
	return !expiresAt.IsZero() && expiresAt.Before(now)
}

func firstFloat(values ...*float64) float64 {
	for _, v := range values {
		if v != nil {
			return *v
		}
	}
	return 0
}
