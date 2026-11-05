//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Chat Completions / Responses 兼容入口必须把 handler 传入的 SessionContext（含
// APIKeyID）交给 openKiroUpstream：同一调用方同一会话 conversationId 稳定，不同
// API Key 不共用 conversationId。
func TestKiroCompatEntriesUseParsedSessionContextForConversationID(t *testing.T) {
	entries := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			body: `{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"same opening"}]}`,
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"claude-sonnet-4-5","stream":false,"input":"same opening"}`,
		},
	}

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			conversationID := func(parsed *ParsedRequest) string {
				upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
				svc := newKiroGatewayForTest(upstream, &rateLimitAccountRepoStub{})
				c, _ := newKiroCompatContext(entry.path)
				// gin 上下文里的 API Key 与 parsed 不同；入口应以 parsed 为准。
				c.Set("api_key", &APIKey{ID: 999})
				var err error
				if entry.name == "chat_completions" {
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, newKiroAccountForTest(), []byte(entry.body), parsed)
				} else {
					_, err = svc.ForwardAsResponses(context.Background(), c, newKiroAccountForTest(), []byte(entry.body), parsed)
				}
				require.NoError(t, err)
				id := gjson.GetBytes(upstream.lastBody, "conversationState.conversationId").String()
				require.NotEmpty(t, id)
				return id
			}

			keyA := &ParsedRequest{SessionContext: &SessionContext{ClientIP: "1.1.1.1", UserAgent: "ua", APIKeyID: 10}}
			keyAAgain := &ParsedRequest{SessionContext: &SessionContext{ClientIP: "1.1.1.1", UserAgent: "ua", APIKeyID: 10}}
			keyB := &ParsedRequest{SessionContext: &SessionContext{ClientIP: "1.1.1.1", UserAgent: "ua", APIKeyID: 20}}

			idA := conversationID(keyA)
			require.Equal(t, idA, conversationID(keyAAgain), "same caller and session must reuse conversationId")
			require.NotEqual(t, idA, conversationID(keyB), "different API keys must not share conversationId")
		})
	}
}
