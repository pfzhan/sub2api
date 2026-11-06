//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStructuredOutputsBetaOAuthMimic(t *testing.T) {
	const beta = "structured-outputs-2025-11-13"
	for _, tc := range []struct {
		name   string
		header string
		drop   map[string]struct{}
		want   bool
	}{
		{"explicit", beta, nil, true},
		{"mixed and duplicate", "custom-beta, " + beta + "," + beta, nil, true},
		{"absent", "custom-beta", nil, false},
		{"similar token", beta + "-other", nil, false},
		{"filtered", beta, map[string]struct{}{beta: {}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestGatewayServiceForBeta(false)
			header := http.Header{}
			header.Set("anthropic-beta", tc.header)
			body := []byte(`{"output_format":{"type":"json_schema","schema":{"type":"object"}}}`)
			got, set := s.computeFinalAnthropicBeta("oauth", true, "claude-sonnet-5", header, body, tc.drop)
			require.True(t, set)
			require.Equal(t, tc.want, containsBetaToken(got, beta))
			require.False(t, containsBetaToken(got, "custom-beta"))
			require.True(t, containsBetaToken(got, "oauth-2025-04-20"))
			if tc.want {
				require.Equal(t, 1, strings.Count(got, beta))
			}
		})
	}
}

func TestCountTokensMimicOmitsIdentityTimeoutAndHelper(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	c.Request.Header.Set("x-stainless-helper-method", "stream")
	c.Request.Header.Set("X-Claude-Code-Session-Id", "client-session")
	c.Request.Header.Set("x-client-request-id", "client-request")
	c.Request.Header.Set("accept-language", "zh-CN")
	account := &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token"},
		Status:      StatusActive,
		Schedulable: true,
	}
	body := []byte(`{"model":"claude-opus-5-5","metadata":{"user_id":"keep-me"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.318.abc; cc_entrypoint=cli;"},{"type":"text","text":"keep prompt"}],"messages":[{"role":"user","content":"hi"}]}`)
	svc := &GatewayService{cfg: &config.Config{}}
	req, wire, err := svc.buildCountTokensRequest(context.Background(), c, account, body, "test-token", "oauth", "claude-opus-5-5", true)
	require.NoError(t, err)
	require.Equal(t, strings.Join(claude.CountTokensMimicryBetas(), ","), getHeaderRaw(req.Header, "anthropic-beta"))
	require.Empty(t, getHeaderRaw(req.Header, "X-Stainless-Timeout"))
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.Empty(t, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
	require.Empty(t, getHeaderRaw(req.Header, "accept-language"))
	require.NotEmpty(t, getHeaderRaw(req.Header, "x-client-request-id"))
	require.NotEqual(t, "client-request", getHeaderRaw(req.Header, "x-client-request-id"))
	require.Equal(t, claude.MimicAcceptEncoding, getHeaderRaw(req.Header, "Accept-Encoding"))
	require.Equal(t, "0.128.0", getHeaderRaw(req.Header, "X-Stainless-Package-Version"))
	require.Equal(t, "Linux", getHeaderRaw(req.Header, "X-Stainless-OS"))
	require.Equal(t, "v26.12.1", getHeaderRaw(req.Header, "X-Stainless-Runtime-Version"))
	require.False(t, gjson.GetBytes(wire, "metadata").Exists())
	require.Equal(t, "keep prompt", gjson.GetBytes(wire, "system.0.text").String())
	require.NotContains(t, string(wire), "x-anthropic-billing-header")

	plain, _ := gin.CreateTestContext(httptest.NewRecorder())
	plain.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	plain.Request.Header.Set("X-Claude-Code-Session-Id", "client-session")
	plain.Request.Header.Set("x-client-request-id", "client-request")
	plainReq, _, err := svc.buildCountTokensRequest(context.Background(), plain, account, []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`), "test-token", "oauth", "claude-opus-5-5", false)
	require.NoError(t, err)
	require.Equal(t, "client-session", getHeaderRaw(plainReq.Header, "X-Claude-Code-Session-Id"))
	require.Equal(t, "client-request", getHeaderRaw(plainReq.Header, "x-client-request-id"))
}

func TestMimicMessagesKeepsTimeoutAndDropsHelper(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("x-stainless-helper-method", "stream")
	applyClaudeCodeMimicHeaders(req, claude.DefaultUserAgent())
	require.Equal(t, "600", getHeaderRaw(req.Header, "X-Stainless-Timeout"))
	require.Empty(t, getHeaderRaw(req.Header, "x-stainless-helper-method"))
	require.Equal(t, claude.MimicAcceptEncoding, getHeaderRaw(req.Header, "Accept-Encoding"))
	require.Equal(t, "application/json", getHeaderRaw(req.Header, "Accept"))
	require.Contains(t, getHeaderRaw(req.Header, "User-Agent"), "claude-cli/"+claude.CLICurrentVersion)
}

func TestStructuredOutputsBetaOAuthMimicKeepsDecemberToken(t *testing.T) {
	const beta = "structured-outputs-2025-12-15"
	s := newTestGatewayServiceForBeta(false)
	header := http.Header{}
	header.Set("anthropic-beta", beta)
	got, set := s.computeFinalAnthropicBeta("oauth", true, "claude-sonnet-5", header, []byte(`{}`), nil)
	require.True(t, set)
	require.True(t, containsBetaToken(got, beta))
	require.Equal(t, 1, strings.Count(got, beta))

	counted, set := s.computeFinalCountTokensAnthropicBeta("oauth", true, "claude-sonnet-5", header, []byte(`{}`), nil)
	require.True(t, set)
	require.False(t, containsBetaToken(counted, beta))
	require.Equal(t, strings.Join(claude.CountTokensMimicryBetas(), ","), counted)
}

func TestBuildUpstreamRequestStructuredOutputsBeta(t *testing.T) {
	const beta = "structured-outputs-2025-11-13"
	for _, filtered := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "policy filtered"}[filtered], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("anthropic-beta", beta+",custom-beta")
			if filtered {
				c.Set(betaPolicyFilterSetKey, map[string]struct{}{beta: {}})
			}
			account := &Account{ID: 701, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token"}, Status: StatusActive, Schedulable: true}
			body := []byte(`{"model":"claude-sonnet-5","max_tokens":1024,"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}},"messages":[{"role":"user","content":"Return JSON"}]}`)
			svc := &GatewayService{cfg: &config.Config{}}
			req, _, err := svc.buildUpstreamRequest(context.Background(), c, account, body,
				"test-token", "oauth", "claude-sonnet-5", false, true)
			require.NoError(t, err)
			out := readUpstreamBodyForTest(t, req)
			require.JSONEq(t, gjson.GetBytes(body, "output_format").Raw, gjson.GetBytes(out, "output_format").Raw)
			header := getHeaderRaw(req.Header, "anthropic-beta")
			require.Equal(t, !filtered, containsBetaToken(header, beta))
			require.False(t, containsBetaToken(header, "custom-beta"))
		})
	}
}
