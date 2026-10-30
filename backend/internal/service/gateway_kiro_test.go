//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type kiroHTTPUpstreamStub struct {
	lastReq  *http.Request
	lastBody []byte
	resp     *http.Response
}

func (s *kiroHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.lastReq = req
	s.lastBody, _ = io.ReadAll(req.Body)
	return s.resp, nil
}

func (s *kiroHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

// kiroEventFrame 按 AWS event stream 格式编码一个 event 帧。
func kiroEventFrame(eventType, payload string) []byte {
	var headers bytes.Buffer
	for _, h := range [][2]string{{":message-type", "event"}, {":event-type", eventType}} {
		headers.WriteByte(byte(len(h[0])))
		headers.WriteString(h[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(h[1])))
		headers.WriteString(h[1])
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(12+headers.Len()+len(payload)+4))
	_ = binary.Write(&buf, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(buf.Bytes()))
	buf.Write(headers.Bytes())
	buf.WriteString(payload)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(buf.Bytes()))
	return buf.Bytes()
}

func kiroEventStreamResponse(frames ...[]byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/vnd.amazon.eventstream"},
			"X-Amzn-Requestid": []string{"kiro-rid"},
		},
		Body: io.NopCloser(bytes.NewReader(bytes.Join(frames, nil))),
	}
}

func kiroJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newKiroGatewayForTest(upstream HTTPUpstream, repo AccountRepository) *GatewayService {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	return &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     NewRateLimitService(repo, nil, cfg, nil, nil),
		deferredService:      &DeferredService{},
		kiroTokenProvider:    NewKiroTokenProvider(nil, nil, nil, nil),
	}
}

func newKiroAccountForTest() *Account {
	return &Account{
		ID:          801,
		Name:        "kiro-test",
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token":  "kiro-at",
			"refresh_token": "kiro-rt",
			"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"auth_method":   "social",
			"region":        "us-east-1",
			"profile_arn":   "arn:aws:codewhisperer:us-east-1:1:profile/p",
		},
	}
}

func forwardKiroForTest(t *testing.T, svc *GatewayService, body string) (*ForwardResult, *httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
	require.NoError(t, err)
	result, err := svc.Forward(context.Background(), c, newKiroAccountForTest(), parsed)
	return result, rec, err
}

func TestGatewayService_ForwardKiroStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
		kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`),
		kiroEventFrame("assistantResponseEvent", `{"content":" world"}`),
		kiroEventFrame("contextUsageEvent", `{"contextUsagePercentage":1}`),
	)}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

	result, rec, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	require.NoError(t, err)

	require.Equal(t, "https://q.us-east-1.amazonaws.com/generateAssistantResponse", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer kiro-at", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:1:profile/p", gjson.GetBytes(upstream.lastBody, "profileArn").String())
	require.Equal(t, "hi", gjson.GetBytes(upstream.lastBody, "conversationState.currentMessage.userInputMessage.content").String())

	out := rec.Body.String()
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, `"text":"Hello"`)
	require.Contains(t, out, `"text":" world"`)
	require.True(t, strings.HasSuffix(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))

	require.Equal(t, "kiro-rid", result.RequestID)
	require.Equal(t, "claude-sonnet-4-5", result.Model)
	require.True(t, result.Stream)
	require.Positive(t, result.Usage.OutputTokens)
	require.Positive(t, result.Usage.InputTokens)
	require.NotNil(t, result.FirstTokenMs)
}

func TestGatewayService_ForwardKiroNonStream(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
		kiroEventFrame("assistantResponseEvent", `{"content":"Hello"}`),
	)}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

	_, rec, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "message", gjson.Get(rec.Body.String(), "type").String())
	require.Equal(t, "Hello", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Equal(t, "end_turn", gjson.Get(rec.Body.String(), "stop_reason").String())
}

func TestGatewayService_ForwardKiroEmptyResponseRetries(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroEventStreamResponse(
		kiroEventFrame("contextUsageEvent", `{"contextUsagePercentage":1}`),
	)}
	svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})

	_, rec, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover))
	require.Equal(t, http.StatusBadGateway, failover.StatusCode)
	require.True(t, failover.RetryableOnSameAccount)
	require.Empty(t, rec.Body.String(), "nothing is written before failover")
}

func TestGatewayService_ForwardKiroQuotaExhaustedFailsOver(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroJSONResponse(http.StatusPaymentRequired,
		`{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`)}
	repo := &rateLimitAccountRepoStub{}
	svc := newKiroGatewayForTest(upstream, repo)

	_, _, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover))
	require.Equal(t, http.StatusPaymentRequired, failover.StatusCode)
	require.Equal(t, "error", gjson.GetBytes(failover.ResponseBody, "type").String())
	require.Equal(t, 1, repo.rateLimitedCalls)
}

func TestGatewayService_ForwardKiroContextLimitReturnsPromptTooLong(t *testing.T) {
	upstream := &kiroHTTPUpstreamStub{resp: kiroJSONResponse(http.StatusBadRequest,
		`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)}
	repo := &rateLimitAccountRepoStub{}
	svc := newKiroGatewayForTest(upstream, repo)

	_, rec, err := forwardKiroForTest(t, svc,
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.True(t, strings.HasPrefix(gjson.Get(rec.Body.String(), "error.message").String(), "prompt is too long"))
	require.Zero(t, repo.rateLimitedCalls)
	require.Zero(t, repo.setErrorCalls)
}
