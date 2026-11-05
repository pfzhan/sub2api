package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 同一账号、同一 TLS profile 下，用量查询按并发 0 取连接池（默认池参数），
// 网关请求按账号并发取。两者不能互相淘汰：网关条目有进行中的请求时不能被删，
// 用量查询之后网关请求仍复用原来的 client。
func TestTLSFingerprintClientNotEvictedByDifferentPoolSettings(t *testing.T) {
	up, ok := NewHTTPUpstream(&config.Config{
		Gateway: config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccountProxy},
	}).(*httpUpstreamService)
	require.True(t, ok)
	profile := &tlsfingerprint.Profile{Name: "Node24", ALPNProtocols: []string{"h2", "http/1.1"}}

	gateway, err := up.acquireClientWithTLS("", 7, 3, profile, service.HTTPUpstreamProfileDefault)
	require.NoError(t, err)

	usage, err := up.acquireClientWithTLS("", 7, 0, profile, service.HTTPUpstreamProfileDefault)
	require.NoError(t, err)
	require.NotSame(t, gateway, usage, "different pool settings need their own client")

	found := false
	for _, entry := range up.clients {
		if entry == gateway {
			found = true
		}
	}
	require.True(t, found, "a client with an in-flight request must stay cached")

	again, err := up.acquireClientWithTLS("", 7, 3, profile, service.HTTPUpstreamProfileDefault)
	require.NoError(t, err)
	require.Same(t, gateway, again, "the next gateway request must reuse its client instead of a new handshake")
}
