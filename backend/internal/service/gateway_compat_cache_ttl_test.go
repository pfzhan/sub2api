//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var compatCacheTTLPattern = regexp.MustCompile(`"ttl"\s*:\s*"([^"]*)"`)

func compatCacheTTLs(body []byte) []string {
	var ttls []string
	for _, m := range compatCacheTTLPattern.FindAllSubmatch(body, -1) {
		ttls = append(ttls, string(m[1]))
	}
	return ttls
}

func compatCacheTTLUpstream() *httpUpstreamRecorder {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
}

// 全局 1h TTL 注入开启时，/v1/chat/completions 与 /v1/responses 转发到 Anthropic OAuth/SetupToken
// 账号的请求体与 /v1/messages 一致：所有 ephemeral 断点都是 1h；关闭时保持默认 TTL。
func TestCompatForwardsFollowGlobalCacheTTL1hInjection(t *testing.T) {
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })
	chatBody := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	responsesBody := []byte(`{"model":"claude-sonnet-4-6","stream":true,"instructions":"You are helpful.","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)

	forwards := map[string]func(svc *GatewayService, c *gin.Context, account *Account) error{
		"chat completions": func(svc *GatewayService, c *gin.Context, account *Account) error {
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(chatBody)))
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, chatBody, nil)
			return err
		},
		"responses": func(svc *GatewayService, c *gin.Context, account *Account) error {
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(responsesBody)))
			_, err := svc.ForwardAsResponses(context.Background(), c, account, responsesBody, nil)
			return err
		},
	}

	for name, forward := range forwards {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
			for _, enabled := range []bool{true, false} {
				label := name + "/" + accountType
				if enabled {
					label += "/injection on"
				} else {
					label += "/injection off"
				}
				t.Run(label, func(t *testing.T) {
					value := "false"
					if enabled {
						value = "true"
					}
					gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
					repo := &gatewayTTLSettingRepo{data: map[string]string{SettingKeyEnableAnthropicCacheTTL1hInjection: value}}
					upstream := compatCacheTTLUpstream()
					svc := &GatewayService{
						cfg:            rawChatCompletionsTestConfig(),
						httpUpstream:   upstream,
						settingService: NewSettingService(repo, &config.Config{}),
					}
					account := &Account{
						ID:          9201,
						Platform:    PlatformAnthropic,
						Type:        accountType,
						Concurrency: 1,
						Credentials: map[string]any{"access_token": "test-token"},
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())

					require.NoError(t, forward(svc, c, account))

					ttls := compatCacheTTLs(upstream.lastBody)
					require.NotEmpty(t, ttls, "the compat path adds its own cache breakpoints")
					for _, ttl := range ttls {
						if enabled {
							require.Equal(t, cacheTTLTarget1h, ttl, string(upstream.lastBody))
						} else {
							require.NotEqual(t, cacheTTLTarget1h, ttl, string(upstream.lastBody))
						}
					}
				})
			}
		}
	}
}
