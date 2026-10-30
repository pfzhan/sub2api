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
)

var (
	forbiddenQuotaPatterns     = []string{"quota", "limit exceeded", "payment required"}
	forbiddenSuspendedPatterns = []string{
		"temporarily is suspended",
		"temporarily suspended",
		"disabled",
		"violation of terms",
		"terms of service",
		"appeal",
		"not authorized to access",
		"not allowed",
	}
)

// ClassifyForbidden 按错误文本判断 403 的原因。
func ClassifyForbidden(body []byte) ForbiddenKind {
	text := strings.ToLower(string(body))
	if text == "" {
		return ForbiddenOther
	}
	for _, p := range forbiddenSuspendedPatterns {
		if strings.Contains(text, p) {
			return ForbiddenSuspended
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
