//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// Messages 流已输出部分文本后中断：客户端断开按断开记账、不动账号；上游读超时
// 不当成客户端断开，在流里报错并按已产出内容记账。
func TestKiroMessagesStreamInterruptAfterOutput(t *testing.T) {
	cases := []struct {
		name           string
		readErr        error
		wantDisconnect bool
	}{
		{name: "client_canceled", readErr: context.Canceled, wantDisconnect: true},
		{name: "upstream_read_timeout", readErr: context.DeadlineExceeded, wantDisconnect: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := kiroEventFrame("assistantResponseEvent", `{"content":"Hello there"}`)
			resp := kiroEventStreamResponse(frame)
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(frame), kiroErrReader{tc.readErr}))
			repo := &rateLimitAccountRepoStub{}
			svc := newKiroGatewayForTest(&kiroHTTPUpstreamStub{resp: resp}, repo)

			result, rec, err := forwardKiroForTest(t, svc,
				`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

			require.NotNil(t, result, "text reached the client, so the request must be billed")
			require.Positive(t, result.Usage.OutputTokens)
			require.Equal(t, tc.wantDisconnect, result.ClientDisconnect)
			require.Contains(t, rec.Body.String(), "Hello there")
			if tc.wantDisconnect {
				require.NoError(t, err)
				require.NotContains(t, rec.Body.String(), "event: error")
			} else {
				require.Error(t, err)
				require.False(t, errors.Is(err, context.Canceled), "a read timeout must not look like a client cancel")
				require.Contains(t, rec.Body.String(), "event: error")
			}
			require.Zero(t, repo.tempCalls, "interrupted streams must not temp-unschedule the account")
			require.Zero(t, repo.setErrorCalls)
			require.Zero(t, repo.rateLimitedCalls)
		})
	}
}
