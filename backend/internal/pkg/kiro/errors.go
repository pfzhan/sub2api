package kiro

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// UpstreamError 是 Kiro 返回的错误体：{"__type": ..., "message": ..., "reason": ...}。
type UpstreamError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// ParseUpstreamError 解析错误体，无法解析时 Message 为原始文本。
func ParseUpstreamError(body []byte) UpstreamError {
	var e UpstreamError
	if err := json.Unmarshal(body, &e); err != nil || (e.Type == "" && e.Message == "" && e.Reason == "") {
		return UpstreamError{Message: strings.TrimSpace(string(body))}
	}
	if e.Message == "" {
		// 部分错误使用大写的 Message 字段。
		var alt struct {
			Message string `json:"Message"`
		}
		if json.Unmarshal(body, &alt) == nil {
			e.Message = alt.Message
		}
	}
	return e
}

// IsContextLimit 报告错误是否为输入超过上下文长度。
func IsContextLimit(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	e := ParseUpstreamError(body)
	message := strings.ToLower(e.Message)
	return strings.EqualFold(e.Reason, "CONTENT_LENGTH_EXCEEDS_THRESHOLD") ||
		strings.Contains(message, "input is too long")
}

// NormalizeStatus 把以 400 返回的限流与容量不足错误改为 429，便于按限流处理
// 并切换账号。
func NormalizeStatus(status int, body []byte) int {
	if status != http.StatusBadRequest {
		return status
	}
	e := ParseUpstreamError(body)
	message := strings.ToLower(e.Message)
	if strings.Contains(strings.ToLower(e.Type), "throttlingexception") ||
		strings.EqualFold(e.Reason, "INSUFFICIENT_MODEL_CAPACITY") ||
		strings.Contains(message, "high traffic") ||
		strings.Contains(message, "try again shortly") {
		return http.StatusTooManyRequests
	}
	return status
}

// ForbiddenKind 是 403 的分类。
type ForbiddenKind int

const (
	// ForbiddenOther 无法判断原因。
	ForbiddenOther ForbiddenKind = iota
	// ForbiddenTokenInvalid 是 access token 过期或失效，刷新后可恢复。
	ForbiddenTokenInvalid
	// ForbiddenQuota 是额度用尽，等额度重置。
	ForbiddenQuota
	// ForbiddenSuspended 是账号被停用或违反条款，需要人工处理。
	ForbiddenSuspended
	// ForbiddenThrottled 是以 403 返回的短时限流，按 429 冷却。
	ForbiddenThrottled
)

// 参考实现只用这些文案区分"能否靠刷新恢复"，命中后标记凭据暂时不健康；这里
// 的后果更重（额度冷却到下月、停用需人工恢复），所以只保留含义明确的文案。
// "not allowed"、"disabled" 等泛化文案也会出现在模型或功能级的 403 里，归为
// ForbiddenOther，由通用逻辑累计计数后再处理。
var (
	forbiddenQuotaPatterns     = []string{"quota", "monthly", "payment required"}
	forbiddenThrottlePatterns  = []string{"rate limit", "too many requests", "throttl"}
	forbiddenSuspendedPatterns = []string{
		"temporarily is suspended",
		"temporarily suspended",
		"violation of terms",
		"terms of service",
		"appeal",
	}
)

// ClassifyForbidden 按错误文本判断 403 的原因。
func ClassifyForbidden(body []byte) ForbiddenKind {
	text := strings.ToLower(string(body))
	if text == "" {
		return ForbiddenOther
	}
	if strings.EqualFold(ParseUpstreamError(body).Reason, "MONTHLY_REQUEST_COUNT") {
		return ForbiddenQuota
	}
	for _, p := range forbiddenSuspendedPatterns {
		if strings.Contains(text, p) {
			return ForbiddenSuspended
		}
	}
	for _, p := range forbiddenThrottlePatterns {
		if strings.Contains(text, p) {
			return ForbiddenThrottled
		}
	}
	for _, p := range forbiddenQuotaPatterns {
		if strings.Contains(text, p) {
			return ForbiddenQuota
		}
	}
	tokenRelated := strings.Contains(text, "token") ||
		strings.Contains(text, "authorization") ||
		strings.Contains(text, "authenticate") ||
		strings.Contains(text, "credential")
	stale := strings.Contains(text, "expired") ||
		strings.Contains(text, "invalid") ||
		strings.Contains(text, "unauthorized")
	if tokenRelated && stale {
		return ForbiddenTokenInvalid
	}
	return ForbiddenOther
}

// QuotaResetAt 返回额度重置时间。Kiro 的月度额度在每月 1 日 00:00 UTC 重置。
func QuotaResetAt(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
