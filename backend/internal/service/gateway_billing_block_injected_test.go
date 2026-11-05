package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// OAuth 改写把 system 注入成 messages 首条后，采样必须跳过注入消息，取真实用户文本。
func TestExtractFirstUserText_SkipsInjectedSystemInstructions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "rewritten_array_blocks",
			body: string(rewriteSystemForNonClaudeCode(
				[]byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":[{"type":"text","text":"real user question"}]}]}`),
				"You are a helpful assistant.",
			)),
			want: "real user question",
		},
		{
			name: "string_content",
			body: `{"messages":[{"role":"user","content":"[System Instructions]\nrules"},{"role":"assistant","content":"ok"},{"role":"user","content":"real string text"}]}`,
			want: "real string text",
		},
		{
			name: "multiple_user_messages_takes_first_real",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"[System Instructions]\nrules"}]},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"first real"}]},{"role":"user","content":"second real"}]}`,
			want: "first real",
		},
		{
			name: "no_injection",
			body: `{"messages":[{"role":"user","content":"plain"}]}`,
			want: "plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractFirstUserText([]byte(tt.body)))
		})
	}
}
