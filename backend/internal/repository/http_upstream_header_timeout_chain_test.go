package repository

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/stretchr/testify/require"
)

// 响应头超时经 http.Client 与 doUpstreamRequest 回到网关时，父 ctx 未取消就不能
// 是 context.Canceled，否则 handleUpstreamTransportError 会当成客户端断开。
// 流式请求的上游 ctx 由 DetachUpstreamContext 派生，两种 ctx 都要覆盖。
func TestTLSFingerprintHeaderTimeoutThroughClientIsNotCanceled(t *testing.T) {
	for name, detach := range map[string]bool{"non_stream": false, "stream_detached": true} {
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			reqCtx := parent
			if detach {
				reqCtx = pkghttputil.DetachUpstreamContext(parent)
			}
			client := &http.Client{Transport: &tlsFingerprintALPNTransport{
				h2:            hangRoundTripper{},
				headerTimeout: 20 * time.Millisecond,
			}}
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "https://upstream.example/v1/messages", nil)
			require.NoError(t, err)

			resp, err := doUpstreamRequest(client, req)

			require.Nil(t, resp)
			require.Error(t, err)
			require.NoError(t, parent.Err(), "the request context is still alive")
			require.False(t, errors.Is(err, context.Canceled), "got %v", err)
			var netErr net.Error
			require.ErrorAs(t, err, &netErr)
			require.True(t, netErr.Timeout())
			require.True(t, isUpstreamTimeoutError(err))
		})
	}
}
