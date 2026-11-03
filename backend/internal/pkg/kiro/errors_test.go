package kiro

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsContextLimit(t *testing.T) {
	t.Parallel()
	require.True(t, IsContextLimit(400, []byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)))
	require.True(t, IsContextLimit(400, []byte(`{"message":"Input is too long for requested model."}`)))
	require.False(t, IsContextLimit(400, []byte(`{"message":"Improperly formed request."}`)))
	require.False(t, IsContextLimit(500, []byte(`{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)))
}

func TestNormalizeStatus(t *testing.T) {
	t.Parallel()
	require.Equal(t, http.StatusTooManyRequests, NormalizeStatus(400, []byte(`{"__type":"com.amazon.aws.codewhisperer#ThrottlingException","message":"Rate exceeded"}`)))
	require.Equal(t, http.StatusTooManyRequests, NormalizeStatus(400, []byte(`{"message":"x","reason":"INSUFFICIENT_MODEL_CAPACITY"}`)))
	require.Equal(t, http.StatusTooManyRequests, NormalizeStatus(400, []byte(`{"message":"We are experiencing high traffic"}`)))
	require.Equal(t, http.StatusBadRequest, NormalizeStatus(400, []byte(`{"message":"Improperly formed request."}`)))
	require.Equal(t, http.StatusInternalServerError, NormalizeStatus(500, []byte(`{"message":"high traffic"}`)))
}

func TestClassifyForbidden(t *testing.T) {
	t.Parallel()
	require.Equal(t, ForbiddenTokenInvalid, ClassifyForbidden([]byte(`{"message":"The bearer token included in the request is invalid."}`)))
	require.Equal(t, ForbiddenTokenInvalid, ClassifyForbidden([]byte(`{"message":"Token has expired"}`)))
	require.Equal(t, ForbiddenSuspended, ClassifyForbidden([]byte(`{"message":"Your User ID temporarily is suspended. Contact support to appeal."}`)))
	require.Equal(t, ForbiddenQuota, ClassifyForbidden([]byte(`{"message":"Monthly request limit exceeded","reason":"MONTHLY_REQUEST_COUNT"}`)))
	require.Equal(t, ForbiddenOther, ClassifyForbidden([]byte(`{"message":"Forbidden"}`)))
	require.Equal(t, ForbiddenQuota, ClassifyForbidden([]byte(`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`)))
	// 短时限流不能按月度额度处理。
	require.Equal(t, ForbiddenThrottled, ClassifyForbidden([]byte(`{"message":"Rate limit exceeded"}`)))
	require.Equal(t, ForbiddenThrottled, ClassifyForbidden([]byte(`{"message":"Too many requests, please wait before trying again."}`)))
	// 模型或功能级的 403 不能直接判为账号停用。
	require.Equal(t, ForbiddenOther, ClassifyForbidden([]byte(`{"message":"User is not allowed to access model claude-opus-4.7"}`)))
	require.Equal(t, ForbiddenOther, ClassifyForbidden([]byte(`{"message":"This feature is disabled"}`)))
}

func TestParseUpstreamError(t *testing.T) {
	t.Parallel()
	e := ParseUpstreamError([]byte(`{"__type":"ValidationException","message":"bad","reason":"R"}`))
	require.Equal(t, UpstreamError{Type: "ValidationException", Message: "bad", Reason: "R"}, e)
	require.Equal(t, "plain text", ParseUpstreamError([]byte("plain text")).Message)
	require.Equal(t, "upper", ParseUpstreamError([]byte(`{"__type":"X","Message":"upper"}`)).Message)
}

func TestQuotaResetAt(t *testing.T) {
	t.Parallel()
	require.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), QuotaResetAt(time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC)))
	require.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), QuotaResetAt(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)))
}
